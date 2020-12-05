package grpc

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/cosmos/cosmos-sdk/server/config"
	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	grpc_logrus "github.com/grpc-ecosystem/go-grpc-middleware/logging/logrus"
	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/improbable-eng/grpc-web/go/grpcweb"
	"github.com/mwitkow/go-conntrack"
	"github.com/mwitkow/grpc-proxy/proxy"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

var (
	flagHttpMaxReadTimeout  = 10 * time.Second
	flagHttpMaxWriteTimeout = 10 * time.Second
)

// StartGRPCProxyServer starts a gRPC-proxy server on the given config.
func StartGRPCProxyServer(grpcConfig config.GRPCConfig) (*http.Server, error) {
	proxyFlags := grpcConfig.GRPCWebProxy
	logrus.SetOutput(os.Stdout)

	logEntry := logrus.NewEntry(logrus.StandardLogger())

	grpcSrv, err := buildGrpcProxyServer(logEntry, proxyFlags, grpcConfig.Address)
	if err != nil {
		return nil, err
	}

	allowedOrigins := makeAllowedOrigins(proxyFlags.AllowedOrigins)
	fmt.Println("Allowed origins = ", allowedOrigins)
	options := []grpcweb.Option{
		grpcweb.WithCorsForRegisteredEndpointsOnly(false),
		grpcweb.WithOriginFunc(makeHttpOriginFunc(allowedOrigins, proxyFlags.AllowAllOrigins)),
	}

	if len(proxyFlags.AllowedHeaders) > 0 {
		options = append(
			options,
			grpcweb.WithAllowedRequestHeaders([]string{"*"}),
		)
	}

	wrappedGrpc := grpcweb.WrapServer(grpcSrv, options...)

	if !proxyFlags.EnableHTTPServer {
		return nil, fmt.Errorf("run_http_server is set to false. Enable for grpcweb proxy to function correctly.")
	}

	// Debug server.
	debugServer := buildServer(wrappedGrpc, proxyFlags)
	http.Handle("/metrics", promhttp.Handler())
	listener, err := buildListenerOrFail("http", proxyFlags.HTTPPort)

	if err != nil {
		return nil, err
	}
	errCh := make(chan error)

	go func() {
		err = debugServer.Serve(listener)
		if err != nil {
			errCh <- fmt.Errorf("failed to serve: %w", err)
		}
	}()

	select {
	case err := <-errCh:
		return nil, err
	case <-time.After(5 * time.Second): // assume server started successfully
		return debugServer, nil
	}

}

func buildServer(wrappedGrpc *grpcweb.WrappedGrpcServer, proxyFlags config.GRPCProxy) *http.Server {
	return &http.Server{
		WriteTimeout: flagHttpMaxWriteTimeout,
		ReadTimeout:  flagHttpMaxReadTimeout,
		Handler: http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
			wrappedGrpc.ServeHTTP(resp, req)
		}),
	}
}

func buildGrpcProxyServer(logger *logrus.Entry, proxyFlags config.GRPCProxy, host string) (*grpc.Server, error) {
	// gRPC-wide changes.
	grpc.EnableTracing = true
	grpc_logrus.ReplaceGrpcLogger(logger)

	// gRPC proxy logic.
	backendConn, err := dialBackendOrFail(proxyFlags, host)
	if err != nil {
		return nil, err
	}
	director := func(ctx context.Context, fullMethodName string) (context.Context, *grpc.ClientConn, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		outCtx, _ := context.WithCancel(ctx)
		mdCopy := md.Copy()
		delete(mdCopy, "user-agent")
		// If this header is present in the request from the web client,
		// the actual connection to the backend will not be established.
		// https://github.com/improbable-eng/grpc-web/issues/568
		delete(mdCopy, "connection")
		outCtx = metadata.NewOutgoingContext(outCtx, mdCopy)
		return outCtx, backendConn, nil
	}
	// Server with logging and monitoring enabled.
	return grpc.NewServer(
		grpc.CustomCodec(proxy.Codec()), // needed for proxy to function.
		grpc.UnknownServiceHandler(proxy.TransparentHandler(director)),
		grpc_middleware.WithUnaryServerChain(
			grpc_logrus.UnaryServerInterceptor(logger),
			grpc_prometheus.UnaryServerInterceptor,
		),
		grpc_middleware.WithStreamServerChain(
			grpc_logrus.StreamServerInterceptor(logger),
			grpc_prometheus.StreamServerInterceptor,
		),
	), nil
}

func buildListenerOrFail(name string, port int) (net.Listener, error) {
	addr := fmt.Sprintf("%s:%d", "0.0.0.0", port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("failed listening for '%v' on %v: %v", name, port, err)
	}
	return conntrack.NewListener(listener,
		conntrack.TrackWithName(name),
		conntrack.TrackWithTcpKeepAlive(20*time.Second),
		conntrack.TrackWithTracing(),
	), nil
}

func makeHttpOriginFunc(allowedOrigins *allowedOrigins, allowAllOrigins bool) func(origin string) bool {
	if allowAllOrigins {
		return func(origin string) bool {
			return true
		}
	}
	return allowedOrigins.IsAllowed
}

func makeAllowedOrigins(origins []string) *allowedOrigins {
	o := map[string]struct{}{}
	for _, allowedOrigin := range origins {
		o[allowedOrigin] = struct{}{}
	}
	return &allowedOrigins{
		origins: o,
	}
}

type allowedOrigins struct {
	origins map[string]struct{}
}

func (a *allowedOrigins) IsAllowed(origin string) bool {
	_, ok := a.origins[origin]
	return ok
}

func dialBackendOrFail(proxyFlags config.GRPCProxy, host string) (*grpc.ClientConn, error) {
	if host == "" {
		return nil, fmt.Errorf("host cannot be empty")
	}

	opt := []grpc.DialOption{}
	opt = append(opt, grpc.WithCodec(proxy.Codec()))

	opt = append(opt, grpc.WithInsecure())

	opt = append(opt,
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(proxyFlags.MaxCallRecvMsgSize)),
		grpc.WithBackoffMaxDelay(proxyFlags.BackendBackoffMaxDelay),
	)

	cc, err := grpc.Dial(host, opt...)
	if err != nil {
		return nil, fmt.Errorf("failed dialing backend: %v", err)
	}
	return cc, nil
}