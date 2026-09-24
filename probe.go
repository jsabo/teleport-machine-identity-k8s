package main

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/go-sql-driver/mysql"
	"github.com/gocql/gocql"
	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
	_ "github.com/sijms/go-ora/v2"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// The tunnels do the authenticating, so no probe carries a real password. Some
// wire protocols still require a non-empty one to send an authentication
// message at all; Teleport checks the user against the certificate and
// discards the password.
const placeholderPassword = "teleport"

// probe opens one connection to the tunnel for t, asks the server who it thinks
// we are and what version it runs, and closes. One session per probe, so the
// audit log shows exactly one db.session.start per row per refresh.
func probe(ctx context.Context, t Target) Result {
	r := Result{Name: t.Name, Engine: t.Engine, Port: t.Port, User: t.User, Database: t.Database}
	start := time.Now()
	var err error
	switch t.Engine {
	case "postgres":
		r.ConnectedAs, r.Version, err = probePostgres(ctx, t)
	case "mysql":
		r.ConnectedAs, r.Version, err = probeMySQL(ctx, t)
	case "mongodb":
		r.ConnectedAs, r.Version, err = probeMongo(ctx, t)
	case "redis":
		r.ConnectedAs, r.Version, err = probeRedis(ctx, t)
	case "clickhouse":
		r.ConnectedAs, r.Version, err = probeClickHouse(ctx, t)
	case "cassandra":
		r.ConnectedAs, r.Version, err = probeCassandra(ctx, t)
	case "oracle":
		r.ConnectedAs, r.Version, err = probeOracle(ctx, t)
	default:
		err = fmt.Errorf("unknown engine %q", t.Engine)
	}
	r.LatencyMS = float64(time.Since(start).Microseconds()) / 1000
	if err != nil {
		r.Error = err.Error()
		return r
	}
	r.OK = true
	return r
}

func addr(t Target) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(t.Port)) }

func probePostgres(ctx context.Context, t Target) (user, version string, err error) {
	dsn := fmt.Sprintf("postgres://%s@%s/%s?sslmode=disable&connect_timeout=4",
		url.PathEscape(t.User), addr(t), url.PathEscape(t.Database))
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return "", "", err
	}
	defer conn.Close(ctx)
	err = conn.QueryRow(ctx, "SELECT current_user, version()").Scan(&user, &version)
	return user, shortVersion(version), err
}

func probeMySQL(ctx context.Context, t Target) (user, version string, err error) {
	cfg := mysql.NewConfig()
	cfg.User = t.User
	cfg.Net = "tcp"
	cfg.Addr = addr(t)
	cfg.DBName = t.Database
	cfg.Timeout = 4 * time.Second
	cfg.AllowNativePasswords = true
	c, err := mysql.NewConnector(cfg)
	if err != nil {
		return "", "", err
	}
	db := sql.OpenDB(c)
	defer db.Close()
	err = db.QueryRowContext(ctx, "SELECT CURRENT_USER(), VERSION()").Scan(&user, &version)
	return user, version, err
}

func probeMongo(ctx context.Context, t Target) (user, version string, err error) {
	uri := fmt.Sprintf("mongodb://%s/?directConnection=true&serverSelectionTimeoutMS=4000&connectTimeoutMS=4000", addr(t))
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		return "", "", err
	}
	defer func() { _ = client.Disconnect(context.Background()) }()
	var status struct {
		AuthInfo struct {
			Users []struct {
				User string `bson:"user"`
				DB   string `bson:"db"`
			} `bson:"authenticatedUsers"`
		} `bson:"authInfo"`
	}
	if err := client.Database("admin").RunCommand(ctx, bson.D{{Key: "connectionStatus", Value: 1}}).Decode(&status); err != nil {
		return "", "", err
	}
	var names []string
	for _, u := range status.AuthInfo.Users {
		names = append(names, u.User+"@"+u.DB)
	}
	user = strings.Join(names, ", ")
	var info struct {
		Version string `bson:"version"`
	}
	if err := client.Database(t.Database).RunCommand(ctx, bson.D{{Key: "buildInfo", Value: 1}}).Decode(&info); err != nil {
		return user, "", err
	}
	return user, "MongoDB " + info.Version, nil
}

func init() {
	// The probe reports its own errors; the pool's retry chatter adds nothing.
	redis.SetLogger(silentLogger{})
}

type silentLogger struct{}

func (silentLogger) Printf(context.Context, string, ...any) {}

func probeRedis(ctx context.Context, t Target) (user, version string, err error) {
	// Redis only authenticates a client that sends AUTH, and a client only sends
	// AUTH when it has a password. Without it the session runs as "default".
	rdb := redis.NewClient(&redis.Options{Addr: addr(t), Username: t.User, Password: placeholderPassword,
		DialTimeout: 4 * time.Second, ReadTimeout: 4 * time.Second, MaxRetries: -1})
	defer rdb.Close()
	user, err = rdb.Do(ctx, "ACL", "WHOAMI").Text()
	if err != nil {
		return "", "", err
	}
	info, err := rdb.Info(ctx, "server").Result()
	if err != nil {
		return user, "", err
	}
	for _, line := range strings.Split(info, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "redis_version:"); ok {
			version = "Redis " + v
		}
	}
	return user, version, nil
}

func probeClickHouse(ctx context.Context, t Target) (user, version string, err error) {
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr:        []string{addr(t)},
		Auth:        clickhouse.Auth{Database: t.Database, Username: t.User},
		DialTimeout: 4 * time.Second,
	})
	if err != nil {
		return "", "", err
	}
	defer conn.Close()
	err = conn.QueryRow(ctx, "SELECT currentUser(), version()").Scan(&user, &version)
	return user, "ClickHouse " + version, err
}

func probeCassandra(ctx context.Context, t Target) (user, version string, err error) {
	cl := gocql.NewCluster("127.0.0.1")
	cl.Port = t.Port
	cl.Authenticator = gocql.PasswordAuthenticator{Username: t.User, Password: placeholderPassword}
	cl.ConnectTimeout = 4 * time.Second
	cl.Timeout = 4 * time.Second
	cl.ProtoVersion = 4
	// The driver would otherwise learn the real node addresses from system.peers
	// and try to dial them directly, bypassing the tunnel.
	cl.DisableInitialHostLookup = true
	cl.Events.DisableNodeStatusEvents = true
	cl.Events.DisableTopologyEvents = true
	cl.Events.DisableSchemaEvents = true
	cl.NumConns = 1
	s, err := cl.CreateSession()
	if err != nil {
		return "", "", err
	}
	defer s.Close()
	err = s.Query("SELECT release_version FROM system.local").WithContext(ctx).Scan(&version)
	// Cassandra here runs with no accounts of its own: the identity is the one
	// Teleport checked against the certificate, which is the username we sent.
	return t.User + " (checked by Teleport)", "Cassandra " + version, err
}

func probeOracle(ctx context.Context, t Target) (user, version string, err error) {
	dsn := fmt.Sprintf("oracle://%s:%s@%s/%s?TIMEOUT=4", url.PathEscape(t.User), placeholderPassword, addr(t), url.PathEscape(t.Database))
	db, err := sql.Open("oracle", dsn)
	if err != nil {
		return "", "", err
	}
	defer db.Close()
	if err := db.QueryRowContext(ctx, "SELECT SYS_CONTEXT('USERENV','SESSION_USER') FROM DUAL").Scan(&user); err != nil {
		return "", "", err
	}
	if err := db.QueryRowContext(ctx, "SELECT banner FROM v$version WHERE ROWNUM = 1").Scan(&version); err != nil {
		version = "(version not readable by this user)"
	}
	return user, version, nil
}

// shortVersion trims PostgreSQL's long banner ("PostgreSQL 16.4 (Debian ...) on
// aarch64-...") to the part a reader wants.
func shortVersion(v string) string {
	if i := strings.Index(v, " ("); i > 0 {
		return v[:i]
	}
	if i := strings.Index(v, " on "); i > 0 {
		return v[:i]
	}
	return v
}
