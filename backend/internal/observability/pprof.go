package observability

import (
	"context"
	"errors"
	"github.com/handlerzzy/feedsystem/internal/logging"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"time"

	"go.uber.org/zap"
)

type PprofServer struct {
	name            string
	server          *http.Server
	shutdownTimeout time.Duration
}

func NewPprofMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	return mux
}

func NewPprofServer(name string, enabled bool, addr string) (*PprofServer, error) {
	if !enabled || addr == "" {
		return nil, nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("failed to start %s pprof server on %s: %w", name, addr, err)
	}
	pprofServer := &PprofServer{
		name:            name,
		shutdownTimeout: 3 * time.Second,
	}
	pprofServer.server = &http.Server{
		Addr:              addr,
		Handler:           NewPprofMux(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	// 此 goroutine 无请求 ctx（服务自身生命周期），故用全局 logger。
	go func() {
		logging.L().Info("pprof listening", zap.String("name", name), zap.String("addr", addr))
		if err := pprofServer.server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// 运行中意外退出且没有重启路径，该端口此后不再可用，故为 Error。
			logging.L().Error("pprof server error", zap.String("name", name), zap.Error(err))
		}
	}()
	return pprofServer, nil
}

func Shutdown(ctx context.Context, srv *http.Server) error {
	if srv == nil {
		return nil
	}
	return srv.Shutdown(ctx)
}

func (s *PprofServer) Close() error {
	if s == nil {
		return nil
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
	defer cancel()
	if err := Shutdown(shutdownCtx, s.server); err != nil {
		// 关闭路径的失败：进程随即退出，错误仍原样返回给调用方，故为 Warn。
		logging.L().Warn("Failed to shutdown pprof server", zap.String("name", s.name), zap.Error(err))
		return err
	}
	return nil
}
