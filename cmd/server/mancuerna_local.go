//go:build server

package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const mancuernaLocalAddress = "127.0.0.1:18883"

// pgx reads service/pass files as well as environment defaults. Local startup
// refuses ambient connection settings and supplies all relevant values itself.
var mancuernaLocalPGVariables = []string{"PGHOST", "PGPORT", "PGDATABASE", "PGUSER", "PGPASSWORD", "PGPASSFILE", "PGAPPNAME", "PGCONNECT_TIMEOUT", "PGSSLMODE", "PGSSLKEY", "PGSSLCERT", "PGSSLSNI", "PGSSLROOTCERT", "PGSSLPASSWORD", "PGTARGETSESSIONATTRS", "PGSERVICE", "PGSERVICEFILE"}
var mancuernaLocalDatabase = regexp.MustCompile(`^mancuerna_[a-z0-9_]*test[a-z0-9_]*$`)
var mancuernaLocalUser = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

func mancuernaLocalRequested() (bool, error) {
	value := os.Getenv("MANCUERNA_LOCAL_ONLY")
	if value == "" {
		return false, nil
	}
	if value != "1" {
		return false, errMancuernaConfig
	}
	return true, nil
}

func localPersonalDSN(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Opaque != "" || u.Fragment != "" || u.User == nil || !mancuernaLocalUser.MatchString(u.User.Username()) || strings.Count(u.Path, "/") != 1 || !mancuernaLocalDatabase.MatchString(strings.TrimPrefix(u.Path, "/")) || strings.Contains(u.Path, "prod") {
		return "", errMancuernaConfig
	}
	if u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" && u.Hostname() != "::1" {
		return "", errMancuernaConfig
	}
	port, err := strconv.ParseUint(u.Port(), 10, 16)
	if err != nil || port == 0 {
		return "", errMancuernaConfig
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", errMancuernaConfig
	}
	for key, values := range query {
		if len(values) != 1 {
			return "", errMancuernaConfig
		}
		switch key {
		case "sslmode":
			if values[0] != "disable" {
				return "", errMancuernaConfig
			}
		case "connect_timeout":
			n, err := strconv.Atoi(values[0])
			if err != nil || n < 1 || n > 10 {
				return "", errMancuernaConfig
			}
		case "host":
			// pgx treats commas as multiple hosts even for a socket path.
			if !filepath.IsAbs(values[0]) || strings.Contains(values[0], ",") {
				return "", errMancuernaConfig
			}
			resolved, err := filepath.EvalSymlinks(values[0])
			if err != nil {
				return "", errMancuernaConfig
			}
			info, err := os.Stat(resolved)
			if err != nil || !info.IsDir() {
				return "", errMancuernaConfig
			}
			query.Set("host", resolved)
		default:
			return "", errMancuernaConfig
		}
	}
	host := u.Hostname()
	if host == "localhost" {
		host = "127.0.0.1"
	}
	u.Host = net.JoinHostPort(host, strconv.FormatUint(port, 10))
	password, _ := u.User.Password()
	u.User = url.UserPassword(u.User.Username(), password)
	query.Set("sslmode", "disable")
	query.Set("passfile", os.DevNull)
	query.Set("connect_timeout", "5")
	query.Set("application_name", "mancuerna-local-test")
	query.Set("target_session_attrs", "any")
	u.RawQuery = query.Encode()
	return u.String(), nil
}

func prepareMancuernaLocalOnly() (*http.Server, func(), error) {
	for _, name := range mancuernaLocalPGVariables {
		if os.Getenv(name) != "" {
			return nil, nil, errMancuernaConfig
		}
	}
	path := os.Getenv("MANCUERNA_CONFIG_FILE")
	if !filepath.IsAbs(path) {
		return nil, nil, errMancuernaConfig
	}
	c, err := readMancuernaConfig(path)
	if err != nil || c.LocalVerifyGuard == nil {
		return nil, nil, errMancuernaConfig
	}
	c.AuthDatabaseURL, err = localPersonalDSN(c.AuthDatabaseURL)
	if err != nil {
		return nil, nil, err
	}
	c.DataDatabaseURL, err = localPersonalDSN(c.DataDatabaseURL)
	if err != nil {
		return nil, nil, err
	}
	router := newServerRouter()
	cleanup, err := mountMancuerna(router, c)
	if err != nil {
		return nil, nil, errMancuernaConfig
	}
	server := &http.Server{Addr: mancuernaLocalAddress, Handler: router, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 45 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10, ErrorLog: log.New(io.Discard, "", 0)}
	return server, cleanup, nil
}

func runMancuernaLocalOnly() error {
	server, cleanup, err := prepareMancuernaLocalOnly()
	if err != nil {
		return err
	}
	defer cleanup()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	completed := make(chan error, 1)
	go func() { completed <- server.ListenAndServe() }()
	select {
	case err := <-completed:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return errMancuernaConfig
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if server.Shutdown(shutdown) != nil {
			_ = server.Close()
			return errMancuernaConfig
		}
		return nil
	}
}
