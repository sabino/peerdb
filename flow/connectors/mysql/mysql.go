package connmysql

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"net"
	"sync/atomic"
	"time"

	"github.com/go-mysql-org/go-mysql/client"
	"github.com/go-mysql-org/go-mysql/mysql"
	"go.temporal.io/sdk/log"
	"google.golang.org/protobuf/proto"

	metadataStore "github.com/PeerDB-io/peerdb/flow/connectors/external_metadata"
	"github.com/PeerDB-io/peerdb/flow/connectors/utils"
	"github.com/PeerDB-io/peerdb/flow/generated/protos"
	"github.com/PeerDB-io/peerdb/flow/internal"
	"github.com/PeerDB-io/peerdb/flow/shared"
)

type MySqlConnector struct {
	*metadataStore.PostgresMetadata
	config        *protos.MySqlConfig
	ssh           utils.SSHTunnel
	conn          atomic.Pointer[client.Conn] // atomic used for internal concurrency, connector interface is not threadsafe
	contexts      chan context.Context
	logger        log.Logger
	rdsAuth       *utils.RDSAuth
	serverVersion string
	bytesRead     atomic.Int64
}

func NewMySqlConnector(ctx context.Context, config *protos.MySqlConfig) (*MySqlConnector, error) {
	pgMetadata, err := metadataStore.NewPostgresMetadata(ctx)
	if err != nil {
		return nil, err
	}
	ssh, err := utils.NewSSHTunnel(ctx, config.SshConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create ssh tunnel: %w", err)
	}
	logger := internal.LoggerFromCtx(ctx)
	var rdsAuth *utils.RDSAuth
	if config.AuthType == protos.MySqlAuthType_MYSQL_IAM_AUTH {
		rdsAuth = &utils.RDSAuth{
			AwsAuthConfig: config.AwsAuth,
		}
		if err := rdsAuth.VerifyAuthConfig(); err != nil {
			logger.Error("failed to verify auth config", slog.Any("error", err))
			return nil, fmt.Errorf("failed to verify auth config: %w", err)
		}
	}
	contexts := make(chan context.Context)
	c := &MySqlConnector{
		PostgresMetadata: pgMetadata,
		config:           config,
		ssh:              ssh,
		conn:             atomic.Pointer[client.Conn]{},
		contexts:         contexts,
		logger:           logger,
		rdsAuth:          rdsAuth,
	}
	go func() {
		ctx := context.Background()
		for {
			var ok bool
			select {
			case <-ctx.Done():
				ctx = context.Background()
				if conn := c.conn.Swap(nil); conn != nil {
					conn.Close()
				}
			case ctx, ok = <-contexts:
				if !ok {
					return
				}
			}
		}
	}()

	return c, nil
}

func (c *MySqlConnector) watchCtx(ctx context.Context) func() {
	c.contexts <- ctx
	return func() {
		c.contexts <- context.Background()
	}
}

func (c *MySqlConnector) Flavor() string {
	switch c.config.Flavor {
	case protos.MySqlFlavor_MYSQL_MYSQL:
		return mysql.MySQLFlavor
	case protos.MySqlFlavor_MYSQL_MARIA:
		return mysql.MariaDBFlavor
	default:
		return "unknown"
	}
}

func (c *MySqlConnector) Close() error {
	var errs []error
	if c.contexts != nil {
		close(c.contexts)
	}
	if conn := c.conn.Swap(nil); conn != nil {
		if err := conn.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if err := c.ssh.Close(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (c *MySqlConnector) ConnectionActive(context.Context) error {
	return nil
}

func (c *MySqlConnector) Dialer() client.Dialer {
	if c.ssh.Client == nil {
		return NewMeteredDialer(&c.bytesRead, (&net.Dialer{Timeout: time.Minute}).DialContext)
	}
	return NewMeteredDialer(&c.bytesRead, c.ssh.Client.DialContext)
}

func (c *MySqlConnector) connect(ctx context.Context) (*client.Conn, error) {
	conn := c.conn.Load()
	if conn == nil {
		argF := []client.Option{func(conn *client.Conn) error {
			if c.config.Compression > 0 {
				conn.SetCapability(mysql.CLIENT_COMPRESS)
			}
			if !c.config.DisableTls {
				config, err := shared.CreateTlsConfig(
					tls.VersionTLS12, c.config.RootCa, c.config.Host, c.config.TlsHost, c.config.SkipCertVerification,
				)
				if err != nil {
					return err
				}
				conn.SetTLSConfig(config)
			}
			return nil
		}}
		config := c.config
		if c.rdsAuth != nil {
			c.logger.Info("Setting up IAM auth for MySQL")
			host := c.config.Host
			if c.config.TlsHost != "" {
				host = c.config.TlsHost
			}
			token, err := utils.GetRDSToken(ctx, utils.RDSConnectionConfig{
				Host: host,
				Port: config.Port,
				User: config.User,
			}, c.rdsAuth, "MYSQL")
			if err != nil {
				return nil, err
			}
			config = proto.CloneOf(config)
			config.Password = token
		}
		var err error
		conn, err = client.ConnectWithDialer(ctx, "", shared.JoinHostPort(config.Host, config.Port),
			config.User, config.Password, config.Database, c.Dialer(), argF...)
		if err != nil {
			return nil, err
		}
		c.conn.Store(conn)
		if err := ctx.Err(); err != nil {
			// need to check if context cancel came in before above Store
			return nil, err
		}
		if _, err := conn.Execute("SET sql_mode = 'ANSI,NO_BACKSLASH_ESCAPES'"); err != nil {
			return nil, fmt.Errorf("failed to set sql_mode to ANSI: %w", err)
		}

		// Set max_execution_time/max_statement_time to 0 (unlimited)
		switch c.Flavor() {
		case mysql.MySQLFlavor:
			// set max_execution_time to unlimited
			if _, err := conn.Execute("SET SESSION max_execution_time=0;"); err != nil {
				var mErr *mysql.MyError
				if errors.As(err, &mErr) && mErr.Code == mysql.ER_UNKNOWN_SYSTEM_VARIABLE {
					// max_execution_time is not supported, ignore the error
					c.logger.Warn("max_execution_time is not supported by the MySQL server, ignoring", slog.Any("error", err))
				} else {
					return nil, fmt.Errorf("failed to set max_execution_time to 0: %w", err)
				}
			}
		case mysql.MariaDBFlavor:
			// set max_statement_time to unlimited
			if _, err := conn.Execute("SET SESSION max_statement_time=0;"); err != nil {
				var mErr *mysql.MyError
				if errors.As(err, &mErr) && mErr.Code == mysql.ER_UNKNOWN_SYSTEM_VARIABLE {
					// max_statement_time is not supported, ignore the error
					c.logger.Warn("max_statement_time is not supported by the MariaDB server, ignoring", slog.Any("error", err))
				} else {
					return nil, fmt.Errorf("failed to set max_statement_time to 0: %w", err)
				}
			}
		}
	}
	return conn, nil
}

// withRetries return an iterable over connections,
// consumer should break out of loop on success or error,
// to retry for mysql.ErrBadConn
func (c *MySqlConnector) withRetries(ctx context.Context) iter.Seq2[*client.Conn, error] {
	return func(yield func(*client.Conn, error) bool) {
		defer c.watchCtx(ctx)()
		for range 3 {
			conn, err := c.connect(ctx)
			if !yield(conn, err) {
				return
			}
			c.conn.CompareAndSwap(conn, nil)
			conn.Close()
		}
	}
}

func (c *MySqlConnector) Execute(ctx context.Context, cmd string, args ...any) (*mysql.Result, error) {
	var connectionErr error
	for conn, err := range c.withRetries(ctx) {
		if err != nil {
			return nil, err
		}

		rs, err := conn.Execute(cmd, args...)
		if err != nil && mysql.ErrorEqual(err, mysql.ErrBadConn) {
			connectionErr = err
			continue
		}
		return rs, err
	}
	return nil, connectionErr
}

func (c *MySqlConnector) ExecuteSelectStreaming(ctx context.Context, cmd string, result *mysql.Result,
	rowCb client.SelectPerRowCallback,
	resultCb client.SelectPerResultCallback,
	args ...any,
) error {
	var connectionErr error
	for conn, err := range c.withRetries(ctx) {
		if err != nil {
			return err
		}

		if len(args) == 0 {
			if err := conn.ExecuteSelectStreaming(cmd, result, rowCb, resultCb); err != nil {
				if mysql.ErrorEqual(err, mysql.ErrBadConn) {
					connectionErr = err
					continue
				}
				return err
			}
		} else {
			stmt, err := conn.Prepare(cmd)
			if err != nil {
				if mysql.ErrorEqual(err, mysql.ErrBadConn) {
					connectionErr = err
					continue
				}
				return err
			}
			err = stmt.ExecuteSelectStreaming(result, rowCb, resultCb, args...)
			_ = stmt.Close()
			if err != nil {
				if mysql.ErrorEqual(err, mysql.ErrBadConn) {
					connectionErr = err
					continue
				}
				return err
			}
		}
		return nil
	}
	return connectionErr
}

func (c *MySqlConnector) GetGtidModeOn(ctx context.Context) (bool, error) {
	if c.Flavor() == mysql.MySQLFlavor {
		rr, err := c.Execute(ctx, "select @@gtid_mode")
		if err != nil {
			return false, err
		}

		gtid_mode, err := rr.GetString(0, 0)
		if err != nil {
			return false, err
		}

		return gtid_mode == "ON", nil
	} else {
		// mariadb always enabled: https://mariadb.com/kb/en/gtid/#using-global-transaction-ids
		cmp, err := c.CompareServerVersion(ctx, "10.0.2")
		return cmp >= 0, err
	}
}

func (c *MySqlConnector) CompareServerVersion(ctx context.Context, version string) (int, error) {
	if c.serverVersion == "" {
		rr, err := c.Execute(ctx, "SELECT version()")
		if err != nil {
			return 0, fmt.Errorf("failed to get server version: %w", err)
		}

		c.serverVersion, err = rr.GetString(0, 0)
		if err != nil {
			return 0, fmt.Errorf("failed to get server version: %w", err)
		}
	}

	cmp, err := mysql.CompareServerVersions(c.serverVersion, version)
	if err != nil {
		return 0, fmt.Errorf("failed to compare server version: %w", err)
	}
	return cmp, nil
}

func (c *MySqlConnector) GetMasterPos(ctx context.Context) (mysql.Position, error) {
	showBinlogStatus := "SHOW BINARY LOG STATUS"
	masterReplaced := "8.4.0" // https://dev.mysql.com/doc/relnotes/mysql/8.4/en/news-8-4-0.html
	if c.config.Flavor == protos.MySqlFlavor_MYSQL_MARIA {
		showBinlogStatus = "SHOW BINLOG STATUS"
		masterReplaced = "10.5.2" // https://mariadb.com/kb/en/show-binlog-status
	}
	if eq, err := c.CompareServerVersion(ctx, masterReplaced); err == nil && eq < 0 {
		showBinlogStatus = "SHOW MASTER STATUS"
	}

	rr, err := c.Execute(ctx, showBinlogStatus)
	if err != nil {
		return mysql.Position{}, fmt.Errorf("failed to %s: %w", showBinlogStatus, err)
	}

	name, _ := rr.GetString(0, 0)
	pos, _ := rr.GetUint(0, 1)
	return mysql.Position{Name: name, Pos: uint32(pos)}, nil
}

func (c *MySqlConnector) GetMasterGTIDSet(ctx context.Context) (mysql.GTIDSet, error) {
	gtidOn, err := c.GetGtidModeOn(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to check gtid mode: %w", err)
	}
	if !gtidOn {
		return nil, errors.New("gtid mode is not enabled")
	}

	var query string
	switch c.Flavor() {
	case mysql.MariaDBFlavor:
		query = "select @@gtid_current_pos"
	default:
		query = "select @@GLOBAL.gtid_executed"
	}
	rr, err := c.Execute(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to %s: %w", query, err)
	}
	gx, err := rr.GetString(0, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to GetString for %s: %w", query, err)
	}
	gset, err := mysql.ParseGTIDSet(c.Flavor(), gx)
	if err != nil {
		return nil, fmt.Errorf("failed to parse GTID from %s: %w", query, err)
	}
	return gset, nil
}

func (c *MySqlConnector) GetVersion(ctx context.Context) (string, error) {
	for conn, err := range c.withRetries(ctx) {
		if err != nil {
			return "", err
		}
		version := conn.GetServerVersion()
		c.logger.Info("[mysql] version", slog.String("version", version))
		return version, nil
	}
	return "", errors.New("failed to connect")
}

func (c *MySqlConnector) StatActivity(
	ctx context.Context,
	req *protos.PostgresPeerActivityInfoRequest,
) (*protos.PeerStatResponse, error) {
	rs, err := c.Execute(ctx, "SELECT ID,COMMAND,STATE,TIME,INFO FROM performance_schema.processlist WHERE USER=?", c.config.User)
	if err != nil {
		// 42S02 is ER_NO_SUCH_TABLE
		var myErr *mysql.MyError
		if errors.As(err, &myErr) && myErr.Code == 1146 && myErr.State == "42S02" {
			// mariadb
			rs, err = c.Execute(ctx,
				"SELECT PROCESSLIST_ID,PROCESSLIST_COMMAND,PROCESSLIST_STATE,PROCESSLIST_TIME,PROCESSLIST_INFO"+
					" FROM performance_schema.threads WHERE USER=?", c.config.User)
			if errors.As(err, &myErr) && myErr.Code == 1146 && myErr.State == "42S02" {
				rs, err = c.Execute(ctx,
					"SELECT ID,COMMAND,STATE,TIME,INFO FROM information_schema.processlist WHERE USER=?", c.config.User)
			}
		}

		if err != nil {
			return nil, err
		}
	}

	statInfoRows := make([]*protos.StatInfo, len(rs.Values))
	for idx, row := range rs.Values {
		statInfoRows[idx] = &protos.StatInfo{
			Pid:           row[0].AsInt64(),
			WaitEvent:     string(row[1].AsString()),
			WaitEventType: "",
			QueryStart:    "",
			Query:         string(row[4].AsString()),
			Duration:      float32(row[3].AsUint64()),
			State:         string(row[2].AsString()),
		}
	}

	return &protos.PeerStatResponse{
		StatData: statInfoRows,
	}, nil
}
