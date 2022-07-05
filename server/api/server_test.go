package api

import (
	"os"
	"testing"

	"google.golang.org/grpc/test/bufconn"
)

const bufSize = 1024 * 1024

var lis *bufconn.Listener

func init() {
	lis = bufconn.Listen(bufSize)

}

//
func TestMain(m *testing.M) {
	os.Setenv("SERVER_PORT", "5000")
	os.Setenv("PASSWORD_SECRET_NAME", "psql-default-password")
	os.Setenv("POSTGRES_HOST", "db")
	os.Setenv("POSTGRES_PORT", "5432")
	os.Setenv("POSTGRES_DB", "transactions")
	os.Setenv("DB_PASSWORD___", "prettystrongpassword")
	os.Setenv("DB_LOGIN", "docker")

	defer os.Unsetenv("SERVER_PORT")
	defer os.Unsetenv("PASSWORD_SECRET_NAME")
	defer os.Unsetenv("POSTGRES_PORT")
	defer os.Unsetenv("POSTGRES_HOST")
	defer os.Unsetenv("POSTGRES_DB")
	defer os.Unsetenv("DB_PASSWORD___")
	defer os.Unsetenv("DB_LOGIN")

	ret := m.Run()
	os.Exit(ret)
}

func TestRun(t *testing.T) {
	// ln, err := net.Listen("tcp", "127.0.0.1")
	// if err != nil {
	// 	t.Error(err)
	// 	t.FailNow()
	// }

}

// func TestTransferServer(t *testing.T) {
// 	ctx := context.Background()
// 	// conn, err := buff
// }
