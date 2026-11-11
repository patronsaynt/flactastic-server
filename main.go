// flactastic-server: a multi-user lossless music server for FLACtastic.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	_ "embed"
	"encoding/base32"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"flactastic/server/api"
	"flactastic/server/library"
	"flactastic/server/store"
)

var version = "beta-1"

//go:embed assets/logo.txt
var logo string

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "flactastic-server:", err)
		os.Exit(1)
	}
}

func serve(o opts) error {
	if isTerminal(1) {
		if w, _ := terminalWidth(); w >= 66 {
			fmt.Print(strings.Trim(logo, "\n"), "\n\n")
		}
		fmt.Printf("FLACtastic Server %s\n", version)
	}
	s, err := store.New(o.data)
	if err != nil {
		return err
	}
	cert, err := certificate(o.data)
	if err != nil {
		return err
	}
	ffmpeg, _ := exec.LookPath("ffmpeg")
	name, _ := os.Hostname()
	libs := library.NewManager(s)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go libs.Run(ctx)
	app := api.New(s, libs, version, name, ffmpeg)
	// The session command asks the running server over this socket.
	sock := filepath.Join(o.data, "admin.sock")
	os.Remove(sock)
	if admin, err := net.Listen("unix", sock); err != nil {
		slog.Warn("no admin socket; the session command will not work", "err", err)
	} else {
		os.Chmod(sock, 0o600)
		defer os.Remove(sock)
		go http.Serve(admin, http.HandlerFunc(app.Sessions))
	}
	srv := &http.Server{
		Addr:              o.listen,
		Handler:           app.Handler(),
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}},
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(slog.Default().Handler(), slog.LevelDebug),
	}
	ln, err := net.Listen("tcp", o.listen)
	if err != nil {
		return err
	}
	slog.Info("listening", "version", version, "addr", o.listen, "fingerprint", fingerprint(cert), "transcoding", ffmpeg != "")
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	if err := srv.ServeTLS(ln, "", ""); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// certificate loads the server's key pair, making one on first use. The
// key never rotates: clients pin it.
func certificate(dir string) (tls.Certificate, error) {
	certFile, keyFile := filepath.Join(dir, "tls", "cert.pem"), filepath.Join(dir, "tls", "key.pem")
	if c, err := tls.LoadX509KeyPair(certFile, keyFile); err == nil {
		return c, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "FLACtastic Server"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(100, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	if err := os.MkdirAll(filepath.Dir(certFile), 0o700); err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return tls.Certificate{}, err
	}
	return tls.LoadX509KeyPair(certFile, keyFile)
}

// fingerprint is the display form of SHA-256(SPKI): 60 bits as XXXX-XXXX-XXXX.
func fingerprint(c tls.Certificate) string {
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	s := base32.StdEncoding.EncodeToString(sum[:])[:12]
	return s[:4] + "-" + s[4:8] + "-" + s[8:]
}

func isTerminal(fd int) bool {
	_, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	return err == nil
}

func terminalWidth() (int, error) {
	ws, err := unix.IoctlGetWinsize(1, unix.TIOCGWINSZ)
	if err != nil {
		return 0, err
	}
	return int(ws.Col), nil
}
