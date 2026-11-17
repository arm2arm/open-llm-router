package oidcclient

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"syscall"
)

type discoveryError struct{ cause error }

func (e *discoveryError) Error() string { return "OIDC discovery failed" }
func (e *discoveryError) Unwrap() error { return e.cause }

type providerStatusError struct{ status int }

func (e *providerStatusError) Error() string {
	return fmt.Sprintf("identity provider returned HTTP %d", e.status)
}

// DiagnosticCause returns only known classifications. Provider bodies, URLs,
// OAuth error descriptions and arbitrary underlying error text are never logged.
func DiagnosticCause(err error) string {
	var status *providerStatusError
	var dns *net.DNSError
	var certificate x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var timeout net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "request timed out"
	case errors.Is(err, context.Canceled):
		return "request canceled"
	case errors.Is(err, ErrIssuerMismatch):
		return "provider issuer does not match configuration"
	case errors.As(err, &status):
		return status.Error()
	case errors.As(err, &dns):
		return "DNS lookup failed"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.As(err, &certificate):
		return "TLS certificate is not trusted"
	case errors.As(err, &hostname):
		return "TLS certificate hostname does not match"
	case errors.As(err, &timeout) && timeout.Timeout():
		return "request timed out"
	default:
		return "provider connection, response or discovery metadata invalid"
	}
}
