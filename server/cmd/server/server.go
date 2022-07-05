// Пакет main запускает сервер с активным подключением к postgresql и rpc методами, описанными в proto/server.proto
package main

import (
	"context"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Grshor/transactional_system/server/api"

	"github.com/jackc/pgx/v4/pgxpool"
)

func toDsn(key, val string) string {
	return key + "=" + val + " "
}

func main() {
	var (
		ctx    context.Context
		cancel context.CancelFunc
	)
	ctx, cancel = context.WithCancel(context.Background())
	go handleSignals(cancel)

	// для сервера
	LISTEN_PORT := os.Getenv("SERVER_PORT")
	LISTEN_HOST := "localhost"
	TLS_CERT := "" // путь к файлу или брать текстовое значение из docker secret
	TLS_KEY := ""  // путь к файлу или брать текстовое значение из docker secret
	WITH_TLS := false
	//для бд
	DB_HOST := os.Getenv("POSTGRES_HOST")
	DB_PORT := os.Getenv("POSTGRES_PORT")
	DB_TABLE := os.Getenv("POSTGRES_DB")
	DB_LOGIN := os.Getenv("DB_LOGIN")

	// dockerSecrets, _ := secrets.NewDockerSecrets("")
	// PASSWORD_SECRET_NAME := os.Getenv("PASSWORD_SECRET_NAME")
	//TODO: не работает!
	// DB_PASSWORD, err := dockerSecrets.Get(PASSWORD_SECRET_NAME)
	// if err != nil {
	// 	log.Fatal(err)
	// }
	// прийдется использовать env
	DB_PASSWORD := os.Getenv("DB_PASSWORD___")
	run(ctx, DB_LOGIN, DB_PASSWORD, DB_HOST, DB_PORT, DB_TABLE, LISTEN_PORT, LISTEN_HOST, TLS_CERT, TLS_KEY, WITH_TLS)
}

// run создана как независимая от переменных окружения и "файлов секрета" вариация функции main, оптимизированная для тестирования
func run(ctx context.Context, DB_LOGIN, DB_PASSWORD, DB_HOST, DB_PORT, DB_TABLE, LISTEN_PORT, LISTEN_HOST, TLS_CERT, TLS_KEY string, WITH_TLS bool) {
	db_dsn := fmt.Sprint(
		toDsn("user", DB_LOGIN),
		toDsn("password", DB_PASSWORD),
		toDsn("host", DB_HOST),
		toDsn("port", DB_PORT),
		toDsn("dbname", DB_TABLE),
	)
	db_config, err := pgxpool.ParseConfig(db_dsn)
	if err != nil {
		log.Fatalf("Failed to build db_config string: %v", err)
	}
	var dbpool *pgxpool.Pool
	for {
		// коннектимся пока не получится
		dbpool, err = pgxpool.ConnectConfig(ctx, db_config)
		if err != nil {
			log.Printf("Failed to establish connection to db: %v", err)
			time.Sleep(time.Second * 10)
		} else {
			break
		}
	}
	defer dbpool.Close()
	// создаем и включаем сервер
	changelogFilepath := "/root/log/unsentChanges.txt"
	var unsentChangesLogfile *os.File
	flagMask := os.O_RDWR | os.O_APPEND | os.O_CREATE
	if unsentChangesLogfile, err = os.OpenFile(changelogFilepath, flagMask, fs.ModeAppend); err != nil {
		_ = os.Chmod(changelogFilepath, 0600) // меняем файл на writable
		if unsentChangesLogfile, err = os.OpenFile(changelogFilepath, flagMask, fs.ModeAppend); err != nil {
			log.Fatal(err)
		}
	}

	errLogger := log.New(os.Stderr, "trsSrr ", log.Lshortfile|log.Lmsgprefix|log.LstdFlags|log.LUTC)
	respLogger := log.New(os.Stdout, "trsSrr ", log.Lmsgprefix|log.LstdFlags|log.LUTC)
	unsentLogger := log.New(unsentChangesLogfile, "trsSrr ", log.LstdFlags|log.LUTC)
	if err != nil {
		log.Fatalf("Failed to open logfile: %v", err)
	}
	unsentLogger.Println("started")
	s := &api.TransactionsServer{PgConn: dbpool, Ctx: ctx, ErrLogger: errLogger, RespLogger: respLogger, UnsentLogger: unsentLogger}
	err = s.Run(LISTEN_HOST, LISTEN_PORT, TLS_CERT, TLS_KEY, WITH_TLS)
	log.Fatal(err)
}

// handleSignals закрывает программу при поступлении в поток выполнения программы сигнала о прекращении работы
func handleSignals(close context.CancelFunc) {
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT)

	exitChan := make(chan int)
	go func() {
		for {
			s := <-signalChan
			switch s {
			case syscall.SIGHUP,
				syscall.SIGINT,
				syscall.SIGTERM,
				syscall.SIGQUIT:
				log.Println("Received shutdown signal:", s)
				exitChan <- 0

			default:
				log.Println("Unknown signal:", s)
				exitChan <- 1
			}
		}
	}()

	code := <-exitChan

	api.GrpcServer.GracefulStop() // закрываем сервер
	close()                       // закрываем остальные процессы
	os.Exit(code)
}
