package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"strings"
	"time"

	pb "github.com/Grshor/transactional_system/server/pkg/proto"

	"github.com/jackc/pgtype"
	"github.com/jackc/pgx/v4"
	"github.com/jackc/pgx/v4/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// TransactionsServer реализует интервейс сервера, описанный в pkg/proto/server.pb.go
type TransactionsServer struct {
	pb.UnimplementedTransactionsServer
	PgConn            *pgxpool.Pool
	Ctx               context.Context
	ErrLogger         *log.Logger
	RespLogger        *log.Logger
	UnsentLogger      *log.Logger
	RequestHeaderName string
}

var GrpcServer *grpc.Server

// Run метод типа *TranscationsServer запускает сервер на указанном хосте/порте,
// возвращает ошибку в случае провала запуска tcp сервера и сервера обработки rpc запросов
func (s *TransactionsServer) Run(host, port, certFile, keyFile string, withTLS bool) error {
	lis, err := net.Listen("tcp", fmt.Sprintf("%s:%s", host, port))
	if err != nil {
		return err
	}
	if withTLS {
		creds, _ := credentials.NewServerTLSFromFile(certFile, keyFile)
		GrpcServer = grpc.NewServer(grpc.Creds(creds))
	} else {
		GrpcServer = grpc.NewServer()
	}
	pb.RegisterTransactionsServer(GrpcServer, s)
	s.RespLogger.Println("Started grpc server!")
	return GrpcServer.Serve(lis)
}

type client struct {
	ID             int
	ClientKey      string
	PrivateKey     string
	PrivateKeySalt string
	Balance        *pgtype.Numeric
}

// ERRORS
var (
	ErrExhausted        = errors.New("database is too busy")
	ErrUnavailable      = errors.New("database is not available")
	ErrPermissionDenied = errors.New("permission denied")
	ErrInvalidArgument  = errors.New("invalid argument")
	ErrAborted          = errors.New("aborted transaction")
	errToCode           = map[error]codes.Code{
		pgx.ErrNoRows:           codes.NotFound,
		pgx.ErrTxClosed:         codes.Aborted,
		pgx.ErrTxCommitRollback: codes.Aborted,
		ErrExhausted:            codes.ResourceExhausted,
		ErrUnavailable:          codes.Unavailable,
		ErrPermissionDenied:     codes.PermissionDenied,
		ErrInvalidArgument:      codes.InvalidArgument,
		ErrAborted:              codes.Aborted,
		nil:                     codes.OK,
	}
)

// ErrToCode выполняет задуманное поведение словаря errToCode
func ErrToCode(err error) codes.Code {
	if code, present := errToCode[err]; present {
		return code
	} else {
		return codes.Unknown
	}
}

// cryptEqual сравнивает, является ли y зашифрованной с солью salt версией x по алгоритму N
// TODO: unimplemented
func cryptEqual(x, y, salt string) bool {
	return x+salt == y //! типа не зашифрованные значения хранятся сейчас в бд
}

// compare сравнивает два pgtype.Numeric
// 1, left>right
// 0, left==right
// -1, left<right
func compare(left, right *pgtype.Numeric) int {
	normLeft := &pgtype.Numeric{Int: (&big.Int{}).Set(left.Int), Status: left.Status}
	normRight := &pgtype.Numeric{Int: (&big.Int{}).Set(right.Int), Status: right.Status}

	if left.Exp < right.Exp {
		mul := (&big.Int{}).Exp(big.NewInt(10), big.NewInt(int64(right.Exp-left.Exp)), nil)
		normRight.Int.Mul(normRight.Int, mul)
	} else if left.Exp > right.Exp {
		mul := (&big.Int{}).Exp(big.NewInt(10), big.NewInt(int64(left.Exp-right.Exp)), nil)
		normLeft.Int.Mul(normLeft.Int, mul)
	}
	return normLeft.Int.Cmp(normRight.Int)
}

// GegerateUserKeys создаёт приватный ключ y и соль salt из ключа x по алгоритму N
//TODO: unimplemented
func generateUserKeys(x string) (y, salt string) { return }

// Transfer реализует серверный rpc метод Transfer сервиса TransactionsServer с двухпоточным стримингом сообщений
// он читает из потока stream сообщения формата pb.TrasferRequest и на каждое отвечает объектом pb.TransferResponse
// каждое принятое сообщение имеет тип proto.TransferRequest с полями {From, To, PrivateKey, Amount, TransferId}
// каждое возвращаемое сообщение имеет тип proto.TransferResponse с полем {StatusCode, TransferId} принимающим значения из перечисления grpc.codes (uint32 от 0 до 16)
//
// транзакция успешна если отправитель есть в базе данных (обращаемой через s.PgConn) есть client_key==From, его balance>= Amount
// возвращаемые значения StatusCode, в зависимости от сценариев исполнения транзакции:
//
// 0. если транзакция прошла успешно
// 1. если клиент отменил запрос
// 2. не классифицируемые случаи
// 3. если Amount не получилось преобразовать в math.big.Float
// 4. при превышении времени ожидания
// 5. если From не был найден в базе
// 6. не будет возвращен
// 7. если PrivateKey не соответствует таковому у существующего в базе From или если Amount недостаточен для проведения транзакции
// 8. в случае перегрузки сервера, или при слишком большом входящем сообщении
// 9. если не удалось установить подключение к базе данных
// 10. если транзакция в базе данных провалится
// 11. не будет возвращен
// 12. при несоответствии версий спецификации
// 13. будет возвращен при ошибке grpc вызовов
// 14. будет возвращен при downtime grpc сервера
// 15. не будет возвращен
// 16. в случае, если сервер запущен в режиме TLS, и клиентом были предоставлены неверные данные для аутентификации подключения
func (s *TransactionsServer) Transfer(stream pb.Transactions_TransferServer) error {
	// нужен префикс в пределах транзакции, а не на весь многопоточный логер
	var prefix = "\t"
	if s.RequestHeaderName != "" {
		md, ok := metadata.FromIncomingContext(stream.Context())
		if ok {
			prefix += md.Get(s.RequestHeaderName)[0] + ":"
		}
	}
	// отлов проблем с подключением к бд
	// мы не будем обслуживать запрос, если мы не смогли взять connection за 10 секунд
	dbConCtx, cancelDbConn := context.WithTimeout(s.Ctx, time.Second*10)
	var err error
	defer cancelDbConn()
	if err = s.PgConn.Ping(s.Ctx); err != nil {
		err = ErrUnavailable
		return status.Error(ErrToCode(err), err.Error())
	}
	var conn *pgxpool.Conn // connection, на котором мы выполним весь текущий rpc вызов
	//! Acquire блокируется, если connection pool заполнен и нет доступного connection, пока оно не будет доступно
	if conn, err = s.PgConn.Acquire(dbConCtx); err != nil {
		err = ErrExhausted
		return status.Error(ErrToCode(err), err.Error())
	}
	// обрабатываем все сообщения
	// TODO: сделать этот цикл асинхронным, используя паттерны pipe
	for {
		req, err := stream.Recv()
		switch err {
		case io.EOF:
			s.RespLogger.Print(prefix, "done")
			return nil
		case nil:
		default:
			s.ErrLogger.Print(prefix, err)
			return err
		}
		resp, err := Process(s.Ctx, req, conn)
		if err != nil {
			s.ErrLogger.Print(prefix, err)
		}
		//обрыв соединения, или пользователь закрыл канал - в любом случае мы должны записать неотправленный resp
		if err := stream.Send(resp); err != nil {
			s.ErrLogger.Print(prefix, err)
			s.UnsentLogger.Println(prefix, resp.TransferId, resp.StatusCode)
			s.RespLogger.Print(prefix, resp.TransferId, resp.StatusCode, "unsent")
			conn.Conn().Close(s.Ctx)
			return err
		}
		s.RespLogger.Print(prefix, resp.TransferId, resp.StatusCode, "success")
	}
}

// Process вызывается для каждого запроса pb.TransferRequest и возвращает соответствующий pb.TransferResponse
func Process(ctx context.Context, transfer *pb.TransferRequest, conn *pgxpool.Conn) (*pb.TransferResponse, error) {
	resp := pb.TransferResponse{StatusCode: pb.TransferResponseStatus(codes.Unknown), TransferId: transfer.TransferId}
	// проверка на странные значения - будем считать успешными транзакциями
	if transfer.From == transfer.To || transfer.Amount == "" {
		resp.StatusCode = pb.TransferResponseStatus(ErrToCode(nil))
		return &resp, nil
	}
	// маршалим transfer.Amount в число (должна быть точка, запятая понята не будет, не вижу смысла нелать так чтобы она была)
	transfer_value := new(pgtype.Numeric)
	err := transfer_value.Set(transfer.Amount)
	if err != nil {
		err_message := fmt.Sprintf("%s failed transfer to %s: couldn't convert `%s` to decimal number", transfer.From, transfer.To, transfer.Amount)
		resp.StatusCode = pb.TransferResponseStatus(ErrToCode(ErrInvalidArgument))
		return &resp, errors.New(err_message)
	}
	if !strings.Contains(transfer.Amount, ".") {
		transfer_value.Exp = 0
	}
	// начинаем транзакцию
	from := client{ClientKey: transfer.From}
	to := client{ClientKey: transfer.To}
	tx, err := conn.Begin(ctx) // может вернуть динамическую ошибку со статичным строковым значением, означающим проблему с подключением к бд
	defer tx.Rollback(ctx)     // ничего не будет если tx.Commit() будет вызван раньше
	if err != nil {
		resp.StatusCode = pb.TransferResponseStatus(ErrToCode(ErrUnavailable))
		return &resp, err
	}
	if err = tx.
		QueryRow(ctx, "select id, balance, private_key, private_key_salt from balances where client_key=$1", from.ClientKey).
		Scan(from.ID, from.Balance, from.PrivateKey, from.PrivateKeySalt); err != nil {
		// клиент не найден или ошибка маршализации значений
		resp.StatusCode = pb.TransferResponseStatus(ErrToCode(err))
		return &resp, nil
	}
	// from.BalanceFlt = big.
	// проверка на право обладания и на размер баланса
	//TODO: не учитывается случай, когда экспонента может быть разной!
	from.Balance.Exp = -10
	transfer_value.Exp = -10
	side := compare(from.Balance, transfer_value)
	if side < 0 || !cryptEqual(transfer.PrivateKey, from.PrivateKey, from.PrivateKeySalt) {
		resp.StatusCode = pb.TransferResponseStatus(ErrToCode(ErrPermissionDenied))
		return &resp, nil
	}
	new_from_balance := sub(from.Balance, transfer_value)
	// err = new_balane.
	//* если trasfer.To не существует - создадим его!
	if err = tx.QueryRow(ctx, "select id, balance from balances where client_key=$1", to.ClientKey).Scan(to.ID, to.Balance); err != nil {
		// клиент не найден
		// создаем нового клиента, наследующего от отправителя отправленную сумму
		pkey, salt := generateUserKeys(from.PrivateKey)
		query := "insert into balances (client_key, private_key, private_key_salt, balance) values ($1,$2,$3,$4)"
		if _, insertionError := tx.Exec(ctx, query, to.ClientKey, pkey, salt, transfer_value); insertionError != nil {
			// если ошибка вставки значит дюпа денег не случилось, значит не нужно вычитать баланс отправителя
			resp.StatusCode = pb.TransferResponseStatus(ErrToCode(ErrAborted))
			return &resp, insertionError
		}
	} else {
		// если пользователь существует
		new_to_balance := sum(to.Balance, transfer_value)
		query := "update balances set balance=$1 where id=$2"
		if _, updateError := tx.Exec(ctx, query, new_to_balance, to.ID); updateError != nil {
			resp.StatusCode = pb.TransferResponseStatus(ErrToCode(ErrAborted))
			return &resp, updateError
		}
	}
	query := "update balances set balance=$1 where id=$2"
	if _, updateError := tx.Exec(ctx, query, new_from_balance, from.ID); updateError != nil {
		resp.StatusCode = pb.TransferResponseStatus(ErrToCode(ErrAborted))
		return &resp, updateError
	}
	// все успешно прошло
	err = tx.Commit(ctx)
	resp.StatusCode = pb.TransferResponseStatus(ErrToCode(err))
	return &resp, err
	// клиент найден, не надо создавать нового
}

// Balance реализует серверный простой rpc метод Balance сервиса TransactionsServer
// TODO: реализовать, задокументировать поведение, написать тест
func (s *TransactionsServer) Balance(ctx context.Context, req *pb.BalanceRequest) (*pb.BalanceResponse, error) {
	// ищем в бд пользователя

	// если нашли - возвращаем

	//если нет
	return &pb.BalanceResponse{}, status.Error(codes.Unimplemented, "unimplemented metod")
}

// sub возвращает left-right
//
// не учитывает нулевых значений и бесконечностей
func sub(left, right *pgtype.Numeric) *pgtype.Numeric {
	normLeft := &pgtype.Numeric{Int: (&big.Int{}).Set(left.Int), Status: left.Status}
	normRight := &pgtype.Numeric{Int: (&big.Int{}).Set(right.Int), Status: right.Status}

	if left.Exp < right.Exp {
		mul := (&big.Int{}).Exp(big.NewInt(10), big.NewInt(int64(right.Exp-left.Exp)), nil)
		normRight.Int.Mul(normRight.Int, mul)
	} else if left.Exp > right.Exp {
		mul := (&big.Int{}).Exp(big.NewInt(10), big.NewInt(int64(left.Exp-right.Exp)), nil)
		normLeft.Int.Mul(normLeft.Int, mul)
	}

	diff := (&big.Int{}).Sub(normLeft.Int, normRight.Int)
	return &pgtype.Numeric{Int: diff, Exp: normLeft.Exp, Status: pgtype.Present}
	// new_balance := &pgtype.Numeric{Int: (&big.Int{}).Set(),status}
}

// sub возвращает left+right
//
// не учитывает нулевых значений и бесконечностей
func sum(left, right *pgtype.Numeric) *pgtype.Numeric {
	normLeft := &pgtype.Numeric{Int: (&big.Int{}).Set(left.Int), Status: left.Status}
	normRight := &pgtype.Numeric{Int: (&big.Int{}).Set(right.Int), Status: right.Status}

	if left.Exp < right.Exp {
		mul := (&big.Int{}).Exp(big.NewInt(10), big.NewInt(int64(right.Exp-left.Exp)), nil)
		normRight.Int.Mul(normRight.Int, mul)
	} else if left.Exp > right.Exp {
		mul := (&big.Int{}).Exp(big.NewInt(10), big.NewInt(int64(left.Exp-right.Exp)), nil)
		normLeft.Int.Mul(normLeft.Int, mul)
	}

	diff := (&big.Int{}).Add(normLeft.Int, normRight.Int)
	return &pgtype.Numeric{Int: diff, Exp: normLeft.Exp, Status: pgtype.Present}
}
