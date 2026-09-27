package docker

import (
	"context"
	"io"
	"net"
	"testing"

	controlapi "github.com/moby/buildkit/api/services/control"
	apitypes "github.com/moby/buildkit/api/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func TestRewriteMobyExporters(t *testing.T) {
	request := &controlapi.SolveRequest{
		Exporters: []*controlapi.Exporter{
			{
				Type: "moby",
				Attrs: map[string]string{
					"name":   "porto-test,porto-test:latest,example.com/team/app:v1",
					"unpack": "false",
				},
			},
			{
				Type:  "local",
				Attrs: map[string]string{"dest": "/tmp/output"},
			},
		},
	}

	rewritten, hasMoby, err := rewriteMobyExporters(request)
	if err != nil {
		t.Fatalf("rewrite moby exporters: %v", err)
	}
	if !hasMoby {
		t.Fatal("moby exporter was not detected")
	}
	if rewritten == request {
		t.Fatal("solve request was mutated instead of cloned")
	}
	if got := rewritten.Exporters[0].Type; got != "image" {
		t.Fatalf("exporter type = %q, want image", got)
	}
	attrs := rewritten.Exporters[0].Attrs
	if got := attrs["name"]; got != "docker.io/library/porto-test:latest,example.com/team/app:v1" {
		t.Fatalf("sanitized image names = %q", got)
	}
	if got := attrs["unpack"]; got != "false" {
		t.Fatalf("explicit unpack = %q, want false", got)
	}
	if got := attrs["dangling-name-prefix"]; got != "moby-dangling" {
		t.Fatalf("dangling prefix = %q", got)
	}
	if got := attrs["danging-name-empty-only"]; got != "true" {
		t.Fatalf("dangling fallback = %q", got)
	}
	if got := rewritten.Exporters[1].Type; got != "local" {
		t.Fatalf("non-moby exporter type = %q", got)
	}
	if got := request.Exporters[0].Type; got != "moby" {
		t.Fatalf("original exporter type = %q", got)
	}
	if _, ok := request.Exporters[0].Attrs["dangling-name-prefix"]; ok {
		t.Fatal("original exporter attributes were mutated")
	}
}

func TestRewriteMobyExporterDefaultsUnpackAndDanglingName(t *testing.T) {
	request := &controlapi.SolveRequest{
		ExporterDeprecated: "moby",
	}

	rewritten, hasMoby, err := rewriteMobyExporters(request)
	if err != nil {
		t.Fatalf("rewrite deprecated moby exporter: %v", err)
	}
	if !hasMoby {
		t.Fatal("deprecated moby exporter was not detected")
	}
	if got := rewritten.ExporterDeprecated; got != "image" {
		t.Fatalf("deprecated exporter type = %q, want image", got)
	}
	if got := rewritten.ExporterAttrsDeprecated["unpack"]; got != "true" {
		t.Fatalf("default unpack = %q, want true", got)
	}
	if got := rewritten.ExporterAttrsDeprecated["name"]; got != "" {
		t.Fatalf("untagged image name = %q, want empty", got)
	}
	if got := rewritten.ExporterAttrsDeprecated["dangling-name-prefix"]; got != "moby-dangling" {
		t.Fatalf("dangling prefix = %q", got)
	}
}

func TestRewriteMobyExporterRejectsDigestName(t *testing.T) {
	request := &controlapi.SolveRequest{
		Exporters: []*controlapi.Exporter{{
			Type: "moby",
			Attrs: map[string]string{
				"name": "porto-test@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			},
		}},
	}

	if _, _, err := rewriteMobyExporters(request); err == nil {
		t.Fatal("moby exporter accepted a digest-qualified build tag")
	}
}

func TestBuildKitControlProxyDelegatesMobyToImageExporter(t *testing.T) {
	upstream := &recordingBuildKitSolveServer{}
	connection, err := newBuildKitControlConnection(buildKitControlTestDialer(t, upstream))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()

	refreshes := 0
	proxy := &buildKitControlProxy{
		client:              controlapi.NewControlClient(connection),
		containerdNamespace: "default",
		imageExported: func() {
			refreshes++
		},
	}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"x-docker-expose-session-uuid", "session-id",
	))
	response, err := proxy.Solve(ctx, &controlapi.SolveRequest{
		Exporters: []*controlapi.Exporter{{
			Type:  "moby",
			Attrs: map[string]string{"name": "porto-test"},
		}},
	})
	if err != nil {
		t.Fatalf("proxy solve: %v", err)
	}
	if got := response.ExporterResponse["containerimage.digest"]; got != "sha256:image" {
		t.Fatalf("image digest response = %q", got)
	}
	if got := response.ExporterResponse["containerimage.config.digest"]; got != "sha256:config" {
		t.Fatalf("config digest response = %q", got)
	}
	if got := response.ExporterResponse["image.name"]; got != "docker.io/library/porto-test:latest" {
		t.Fatalf("image name response = %q", got)
	}
	if refreshes != 1 {
		t.Fatalf("image index refreshes = %d, want 1", refreshes)
	}
	if upstream.request == nil || upstream.request.Exporters[0].Type != "image" {
		t.Fatalf("upstream request = %+v", upstream.request)
	}
	if got := upstream.request.Exporters[0].Attrs["unpack"]; got != "true" {
		t.Fatalf("upstream unpack = %q, want true", got)
	}
	if values := upstream.metadata.Get("x-docker-expose-session-uuid"); len(values) != 1 || values[0] != "session-id" {
		t.Fatalf("upstream metadata = %v", upstream.metadata)
	}
}

func TestBuildKitControlProxyRejectsMobyWithoutPortoContainerdWorker(t *testing.T) {
	upstream := &recordingBuildKitSolveServer{
		workerExecutor:  "oci",
		workerNamespace: "buildkit",
	}
	connection, err := newBuildKitControlConnection(buildKitControlTestDialer(t, upstream))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()

	proxy := &buildKitControlProxy{
		client:              controlapi.NewControlClient(connection),
		containerdNamespace: "default",
	}
	_, err = proxy.Solve(context.Background(), &controlapi.SolveRequest{
		Exporters: []*controlapi.Exporter{{Type: "moby"}},
	})
	if err == nil {
		t.Fatal("moby exporter accepted a worker outside Porto's containerd namespace")
	}
	if upstream.solveCalls != 0 {
		t.Fatalf("incompatible worker received %d solve calls", upstream.solveCalls)
	}
}

func TestBuildKitControlProxyCoversControlService(t *testing.T) {
	const forwardedMethods = 9
	actual := len(controlapi.Control_ServiceDesc.Methods) + len(controlapi.Control_ServiceDesc.Streams)
	if actual != forwardedMethods {
		t.Fatalf("BuildKit Control RPCs = %d, update buildKitControlProxy for the new service surface", actual)
	}
}

func TestBuildKitProxyForwardsUnknownServices(t *testing.T) {
	upstreamListener := bufconn.Listen(1024 * 1024)
	upstreamServer := grpc.NewServer()
	upstreamServer.RegisterService(&grpc.ServiceDesc{
		ServiceName: "porto.test.Echo",
		HandlerType: (*buildKitEchoService)(nil),
		Streams: []grpc.StreamDesc{{
			StreamName:    "Exchange",
			ClientStreams: true,
			ServerStreams: true,
			Handler: func(_ any, stream grpc.ServerStream) error {
				request := &wrapperspb.StringValue{}
				if err := stream.RecvMsg(request); err != nil {
					return err
				}
				return stream.SendMsg(&wrapperspb.StringValue{Value: "echo:" + request.Value})
			},
		}},
	}, struct{}{})
	go func() {
		_ = upstreamServer.Serve(upstreamListener)
	}()
	t.Cleanup(func() {
		upstreamServer.Stop()
		_ = upstreamListener.Close()
	})
	upstreamConnection, err := grpc.NewClient(
		"passthrough:///upstream",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return upstreamListener.DialContext(ctx)
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer upstreamConnection.Close()

	proxyListener := bufconn.Listen(1024 * 1024)
	proxyServer := grpc.NewServer(grpc.UnknownServiceHandler(transparentBuildKitHandler(upstreamConnection)))
	go func() {
		_ = proxyServer.Serve(proxyListener)
	}()
	t.Cleanup(func() {
		proxyServer.Stop()
		_ = proxyListener.Close()
	})
	proxyConnection, err := grpc.NewClient(
		"passthrough:///proxy",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return proxyListener.DialContext(ctx)
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer proxyConnection.Close()

	stream, err := proxyConnection.NewStream(context.Background(), buildKitProxyStream, "/porto.test.Echo/Exchange")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SendMsg(&wrapperspb.StringValue{Value: "buildkit"}); err != nil {
		t.Fatal(err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	response := &wrapperspb.StringValue{}
	if err := stream.RecvMsg(response); err != nil {
		t.Fatal(err)
	}
	if response.Value != "echo:buildkit" {
		t.Fatalf("unknown service response = %q", response.Value)
	}
	if err := stream.RecvMsg(&wrapperspb.StringValue{}); err != io.EOF {
		t.Fatalf("unknown service completion = %v, want EOF", err)
	}
}

type buildKitEchoService interface{}

type recordingBuildKitSolveServer struct {
	controlapi.UnimplementedControlServer
	request         *controlapi.SolveRequest
	metadata        metadata.MD
	workerExecutor  string
	workerNamespace string
	solveCalls      int
}

func (s *recordingBuildKitSolveServer) ListWorkers(context.Context, *controlapi.ListWorkersRequest) (*controlapi.ListWorkersResponse, error) {
	executor := s.workerExecutor
	if executor == "" {
		executor = "containerd"
	}
	namespace := s.workerNamespace
	if namespace == "" {
		namespace = "default"
	}
	return &controlapi.ListWorkersResponse{Record: []*apitypes.WorkerRecord{{
		Labels: map[string]string{
			"org.mobyproject.buildkit.worker.executor":             executor,
			"org.mobyproject.buildkit.worker.containerd.namespace": namespace,
		},
	}}}, nil
}

func (s *recordingBuildKitSolveServer) Solve(ctx context.Context, request *controlapi.SolveRequest) (*controlapi.SolveResponse, error) {
	s.solveCalls++
	s.request = request
	s.metadata, _ = metadata.FromIncomingContext(ctx)
	return &controlapi.SolveResponse{ExporterResponse: map[string]string{
		"containerimage.digest":        "sha256:image",
		"containerimage.config.digest": "sha256:config",
		"image.name":                   request.Exporters[0].Attrs["name"],
	}}, nil
}

var _ controlapi.ControlServer = (*recordingBuildKitSolveServer)(nil)
