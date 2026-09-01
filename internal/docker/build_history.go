package docker

import (
	"context"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"time"

	controlapi "github.com/moby/buildkit/api/services/control"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func (m *Manager) Builds(ctx context.Context) ([]Build, error) {
	connection, err := grpc.NewClient(
		"passthrough:///buildkit",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(dialContext context.Context, _ string) (net.Conn, error) {
			return m.DialBuildKit(dialContext)
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("create BuildKit history client: %w", err)
	}
	defer connection.Close()

	stream, err := controlapi.NewControlClient(connection).ListenBuildHistory(ctx, &controlapi.BuildHistoryRequest{
		EarlyExit: true,
		Limit:     100,
	})
	if err != nil {
		return nil, fmt.Errorf("open BuildKit history: %w", err)
	}
	records := make(map[string]*controlapi.BuildHistoryRecord)
	for {
		event, receiveErr := stream.Recv()
		if receiveErr != nil {
			if receiveErr == io.EOF {
				break
			}
			return nil, fmt.Errorf("read BuildKit history: %w", receiveErr)
		}
		if event.Record == nil || strings.TrimSpace(event.Record.Ref) == "" {
			continue
		}
		if event.Type == controlapi.BuildHistoryEventType_DELETED {
			delete(records, event.Record.Ref)
			continue
		}
		records[event.Record.Ref] = event.Record
	}

	builds := make([]Build, 0, len(records))
	for _, record := range records {
		builds = append(builds, decodeBuildHistory(record))
	}
	sort.Slice(builds, func(left, right int) bool {
		return builds[left].CreatedAt > builds[right].CreatedAt
	})
	return builds, nil
}

func decodeBuildHistory(record *controlapi.BuildHistoryRecord) Build {
	build := Build{
		ID:       record.Ref,
		Name:     buildHistoryName(record),
		Status:   "running",
		Platform: firstNonEmpty(record.FrontendAttrs["platform"], record.FrontendAttrs["platforms"]),
	}
	if record.CreatedAt != nil {
		createdAt := record.CreatedAt.AsTime()
		build.CreatedAt = createdAt.Format(time.RFC3339)
		if record.CompletedAt != nil {
			duration := record.CompletedAt.AsTime().Sub(createdAt)
			if duration >= 0 {
				build.Duration = duration.Round(time.Millisecond).String()
			}
		}
	}
	if record.CompletedAt != nil {
		build.Status = "succeeded"
	}
	if record.Error != nil && (record.Error.Code != 0 || record.Error.Message != "") {
		build.Status = "failed"
	}
	return build
}

func buildHistoryName(record *controlapi.BuildHistoryRecord) string {
	for _, exporter := range record.Exporters {
		if name := strings.TrimSpace(exporter.Attrs["name"]); name != "" {
			return strings.Split(name, ",")[0]
		}
	}
	for _, key := range []string{"containerimage.name", "image.name", "name"} {
		if name := strings.TrimSpace(record.ExporterResponse[key]); name != "" {
			return strings.Split(name, ",")[0]
		}
	}
	return firstNonEmpty(
		record.FrontendAttrs["target"],
		record.FrontendAttrs["filename"],
		record.Ref,
	)
}
