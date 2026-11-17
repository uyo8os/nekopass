package agent

import (
	"context"
	"errors"
	"log/slog"
	"time"

	pb "github.com/nekopass/nekopass/internal/protocol"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

func Run(ctx context.Context, e *Engine, server, token string) error {
	if token == "" || server == "" {
		return errors.New("server and node token required")
	}
	origin := server
	server, err := e.redirectedControlEndpoint(origin)
	if err != nil {
		return err
	}
	c, err := e.state.Config()
	if err != nil {
		return err
	}
	if err = e.Apply(c); err != nil {
		return err
	}
	go e.collectProbe(ctx)
	for ctx.Err() == nil {
		target, transport, err := controlEndpoint(server)
		if err != nil {
			return err
		}
		conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(transport), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(16<<20), grpc.MaxCallSendMsgSize(16<<20)))
		if err != nil {
			return err
		}
		err = runSession(ctx, e, pb.NewControlClient(conn), token, server)
		conn.Close()
		if ctx.Err() != nil {
			break
		}
		var redirect *controlRedirect
		if errors.As(err, &redirect) {
			if err := e.saveControlEndpoint(origin, redirect.endpoint); err != nil {
				return err
			}
			server = redirect.endpoint
			slog.Info("control endpoint changed; reconnecting", "server", server)
			continue
		}
		slog.Warn("control connection interrupted; retaining configuration and finite allowance", "error", err)
		// Persist usage even while disconnected.
		if _, err = e.Report(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
	return nil
}
func runSession(parent context.Context, e *Engine, client pb.ControlClient, token, server string) error {
	ctx, cancel := context.WithCancel(metadata.AppendToOutgoingContext(parent, "authorization", "Bearer "+token))
	defer cancel()
	stream, err := client.Connect(ctx)
	if err != nil {
		return err
	}
	diagnostics := newDiagnosticSession(ctx, e)
	// A reconnect always starts with an empty cache; persisted configuration is
	// intentionally not used to negotiate a delta with a different panel.
	var configs streamConfigCache
	for {
		report, err := e.Report()
		if err != nil {
			return err
		}
		report.DiagnosticResults = diagnostics.results()
		report.SupportsConfigDelta = true
		// Apply reports listener and tunnel failures through Error rather than its
		// return value. Do not acknowledge a configuration until it is healthy;
		// requesting a full frame also preserves the existing retry behavior.
		report.AppliedConfigToken = configs.appliedToken(report.Error)
		// Cancel an unresponsive stream rather than trusting TCP keepalive indefinitely.
		watchdog := time.AfterFunc(8*time.Second, cancel)
		if err = stream.Send(report); err != nil {
			watchdog.Stop()
			return err
		}
		diagnostics.sent(report.DiagnosticResults)
		config, err := stream.Recv()
		watchdog.Stop()
		if err != nil {
			return err
		}
		config, token, static, err := configs.materialize(config)
		if err != nil {
			return err
		}
		tasks := config.DiagnosticTasks
		config.DiagnosticTasks = nil // Diagnostic requests are ephemeral, never persist them with the node configuration.
		if err = e.Apply(config); err != nil {
			return err
		}
		configs.token, configs.static = token, static
		diagnostics.schedule(config, tasks)
		if config.ControlEndpoint != "" && config.ControlEndpoint != server {
			if _, _, err := controlEndpoint(config.ControlEndpoint); err != nil {
				return err
			}
			return &controlRedirect{endpoint: config.ControlEndpoint}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// streamConfigCache only records a configuration once Apply has succeeded.
// The dynamic fields in every response (especially user quota, usage ACKs,
// restore, diagnostic tasks and update requests) always come from that response.
type streamConfigCache struct {
	token  string
	static *pb.ControlMessage
}

func (cache *streamConfigCache) appliedToken(syncError string) string {
	if syncError != "" {
		return ""
	}
	return cache.token
}

func (cache *streamConfigCache) materialize(in *pb.ControlMessage) (*pb.ControlMessage, string, *pb.ControlMessage, error) {
	if in == nil {
		return nil, "", nil, errors.New("empty control response")
	}
	token := in.ConfigToken
	var static *pb.ControlMessage
	if in.ConfigUnchanged {
		if token == "" || token != cache.token || cache.static == nil {
			return nil, "", nil, errors.New("configuration delta has no matching applied configuration")
		}
		if in.Node != nil || len(in.Rules) != 0 || len(in.TunnelLinks) != 0 || len(in.EgressRules) != 0 {
			return nil, "", nil, errors.New("configuration delta contains static fields")
		}
		static = cache.static
		copy := proto.Clone(static).(*pb.ControlMessage)
		in.Node, in.Rules, in.TunnelLinks, in.EgressRules = copy.Node, copy.Rules, copy.TunnelLinks, copy.EgressRules
	} else if token != "" {
		static = proto.Clone(&pb.ControlMessage{Node: in.Node, Rules: in.Rules, TunnelLinks: in.TunnelLinks, EgressRules: in.EgressRules}).(*pb.ControlMessage)
	}
	// These markers are transport metadata; persisting them would unnecessarily
	// dirty the state file and make a session-specific token look durable.
	in.ConfigToken = ""
	in.ConfigUnchanged = false
	return in, token, static, nil
}
