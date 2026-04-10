package middleware

import (
	"context"
	"time"

	apm "github.com/RomaLytar/yammiApmSdkGo"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// GRPCUnary returns a unary server interceptor that records each RPC as
// an apm.Event. Mounting it:
//
//	srv := grpc.NewServer(
//	    grpc.UnaryInterceptor(middleware.GRPCUnary(a)),
//	)
//
// Mapping decisions made here:
//
//   - The endpoint string is the gRPC FullMethod (e.g. "/board.BoardService/Create"),
//     which the dashboard already parses as a path.
//
//   - The HTTP method is hard-coded to "POST". The backend's allowed_methods
//     map only accepts standard HTTP verbs, and POST is the closest semantic
//     match for "RPC call with a request body". This keeps gRPC traces in
//     the same table as HTTP traces with no schema changes.
//
//   - The status code is the HTTP equivalent of the gRPC status, derived
//     by httpStatusFromCode(). OK → 200, NOT_FOUND → 404, …
func GRPCUnary(a *apm.APM) grpc.UnaryServerInterceptor {
	if a == nil {
		return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			return handler(ctx, req)
		}
	}

	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		durationMs := float32(time.Since(start).Seconds() * 1000)

		st, _ := status.FromError(err)
		code := st.Code()
		errMsg := ""
		if code != codes.OK {
			errMsg = trim(st.Message(), 500)
		}

		a.PushEvent(apm.Event{
			Endpoint:   info.FullMethod,
			Method:     "POST",
			StatusCode: httpStatusFromCode(code),
			DurationMs: durationMs,
			Error:      errMsg,
			ClientIP:   peerIP(ctx),
			UserAgent:  "grpc-go",
			Timestamp:  time.Now().Unix(),
		})

		return resp, err
	}
}

// GRPCStream is the streaming variant. Each RPC produces exactly one event
// at end-of-stream, regardless of how many messages were exchanged.
// Per-message accounting belongs in the user's code, not in middleware.
func GRPCStream(a *apm.APM) grpc.StreamServerInterceptor {
	if a == nil {
		return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			return handler(srv, ss)
		}
	}

	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		start := time.Now()
		err := handler(srv, ss)
		durationMs := float32(time.Since(start).Seconds() * 1000)

		st, _ := status.FromError(err)
		code := st.Code()
		errMsg := ""
		if code != codes.OK {
			errMsg = trim(st.Message(), 500)
		}

		a.PushEvent(apm.Event{
			Endpoint:   info.FullMethod,
			Method:     "POST",
			StatusCode: httpStatusFromCode(code),
			DurationMs: durationMs,
			Error:      errMsg,
			ClientIP:   peerIP(ss.Context()),
			UserAgent:  "grpc-go",
			Timestamp:  time.Now().Unix(),
		})

		return err
	}
}

// httpStatusFromCode is the canonical gRPC → HTTP mapping that the
// official grpc-gateway uses. Keeping it here means we have one
// dependency-free place to tweak if we ever want a different mapping.
func httpStatusFromCode(c codes.Code) uint16 {
	switch c {
	case codes.OK:
		return 200
	case codes.Canceled:
		return 499
	case codes.Unknown:
		return 500
	case codes.InvalidArgument:
		return 400
	case codes.DeadlineExceeded:
		return 504
	case codes.NotFound:
		return 404
	case codes.AlreadyExists:
		return 409
	case codes.PermissionDenied:
		return 403
	case codes.ResourceExhausted:
		return 429
	case codes.FailedPrecondition:
		return 400
	case codes.Aborted:
		return 409
	case codes.OutOfRange:
		return 400
	case codes.Unimplemented:
		return 501
	case codes.Internal:
		return 500
	case codes.Unavailable:
		return 503
	case codes.DataLoss:
		return 500
	case codes.Unauthenticated:
		return 401
	default:
		return 500
	}
}

// peerIP extracts the caller IP from a gRPC context. Similar trade-offs
// to the HTTP middleware: we trim the port and don't resolve names.
func peerIP(ctx context.Context) string {
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		addr := p.Addr.String()
		for i := len(addr) - 1; i >= 0; i-- {
			if addr[i] == ':' {
				return addr[:i]
			}
		}
		return addr
	}
	return ""
}
