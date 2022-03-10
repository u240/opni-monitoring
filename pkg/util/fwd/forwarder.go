package fwd

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/utils"
	"github.com/rancher/opni-monitoring/pkg/logger"
	"github.com/valyala/fasthttp"
	"go.uber.org/zap"
)

type ForwarderOptions struct {
	logger    *zap.SugaredLogger
	tlsConfig *tls.Config
	name      string
}

type ForwarderOption func(*ForwarderOptions)

func (o *ForwarderOptions) Apply(opts ...ForwarderOption) {
	for _, op := range opts {
		op(o)
	}
}

func WithLogger(logger *zap.SugaredLogger) ForwarderOption {
	return func(o *ForwarderOptions) {
		o.logger = logger
	}
}

func WithName(name string) ForwarderOption {
	return func(o *ForwarderOptions) {
		o.name = strings.TrimSpace(name) + " "
	}
}

func WithTLS(tlsConfig *tls.Config) ForwarderOption {
	return func(o *ForwarderOptions) {
		o.tlsConfig = tlsConfig
	}
}

func To(addr string, opts ...ForwarderOption) func(*fiber.Ctx) error {
	defaultLogger := logger.New().Named("fwd")
	options := &ForwarderOptions{
		logger: defaultLogger,
	}
	options.Apply(opts...)

	if options.name != "" {
		defaultLogger = defaultLogger.Named(options.name)
	}

	hostClient := &fasthttp.HostClient{
		MaxConnWaitTimeout:       2 * time.Second,
		MaxConns:                 4096,
		NoDefaultUserAgentHeader: true,
		DisablePathNormalizing:   true,
		Addr:                     addr,
		IsTLS:                    options.tlsConfig != nil,
		TLSConfig:                options.tlsConfig,
	}

	return func(c *fiber.Ctx) error {
		options.logger.With(
			"method", c.Method(),
			"path", c.Path(),
			"to", addr,
		).Debug("forwarding request")

		req := c.Request()
		resp := c.Response()
		req.Header.Del(fiber.HeaderConnection)
		req.SetRequestURI(utils.UnsafeString(req.RequestURI()))
		if err := hostClient.Do(req, resp); err != nil {
			options.logger.With(
				zap.Error(err),
				"req", c.Path(),
			).Error("error forwarding request")
			return fmt.Errorf("error forwarding request: %w", err)
		}
		resp.Header.Del(fiber.HeaderConnection)
		if resp.StatusCode() != http.StatusOK {
			options.logger.With(
				"response", string(resp.Body()),
				"req", c.Path(),
			).Error("error forwarding request")
		}
		return nil
	}
}