package docker

import (
	"errors"
	"strings"

	"github.com/distribution/reference"
	controlapi "github.com/moby/buildkit/api/services/control"
	"google.golang.org/protobuf/proto"
)

const (
	buildKitMobyExporter         = "moby"
	buildKitImageExporter        = "image"
	buildKitMobyDanglingPrefix   = "moby-dangling"
	buildKitExporterNameKey      = "name"
	buildKitExporterUnpackKey    = "unpack"
	buildKitExporterDanglingKey  = "dangling-name-prefix"
	buildKitExporterDanglingOnly = "danging-name-empty-only"

	buildKitWorkerExecutorLabel            = "org.mobyproject.buildkit.worker.executor"
	buildKitWorkerContainerdNamespaceLabel = "org.mobyproject.buildkit.worker.containerd.namespace"
	buildKitWorkerContainerdExecutor       = "containerd"
)

func rewriteMobyExporters(request *controlapi.SolveRequest) (*controlapi.SolveRequest, bool, error) {
	rewritten := proto.Clone(request).(*controlapi.SolveRequest)
	hasMoby := false

	if rewritten.ExporterDeprecated == buildKitMobyExporter {
		attrs, err := mobyExporterAttrs(rewritten.ExporterAttrsDeprecated)
		if err != nil {
			return nil, false, err
		}
		rewritten.ExporterDeprecated = buildKitImageExporter
		rewritten.ExporterAttrsDeprecated = attrs
		hasMoby = true
	}

	for _, exporter := range rewritten.Exporters {
		if exporter.GetType() != buildKitMobyExporter {
			continue
		}
		attrs, err := mobyExporterAttrs(exporter.GetAttrs())
		if err != nil {
			return nil, false, err
		}
		exporter.Type = buildKitImageExporter
		exporter.Attrs = attrs
		hasMoby = true
	}

	return rewritten, hasMoby, nil
}

func mobyExporterAttrs(attrs map[string]string) (map[string]string, error) {
	rewritten := make(map[string]string, len(attrs)+3)
	for key, value := range attrs {
		rewritten[key] = value
	}

	names, err := sanitizeMobyImageNames(strings.Split(rewritten[buildKitExporterNameKey], ","))
	if err != nil {
		return nil, err
	}
	rewritten[buildKitExporterNameKey] = strings.Join(names, ",")
	if _, ok := rewritten[buildKitExporterUnpackKey]; !ok {
		rewritten[buildKitExporterUnpackKey] = "true"
	}
	if _, ok := rewritten[buildKitExporterDanglingKey]; !ok {
		rewritten[buildKitExporterDanglingKey] = buildKitMobyDanglingPrefix
	}
	rewritten[buildKitExporterDanglingOnly] = "true"
	return rewritten, nil
}

func sanitizeMobyImageNames(names []string) ([]string, error) {
	unique := make(map[string]struct{}, len(names))
	result := make([]string, 0, len(names))
	for _, name := range names {
		if name == "" {
			continue
		}
		parsed, err := reference.ParseNormalizedNamed(name)
		if err != nil {
			return nil, err
		}
		if _, ok := parsed.(reference.Digested); ok {
			return nil, errors.New("build tag cannot contain a digest")
		}
		tagged := reference.TagNameOnly(parsed).String()
		if _, ok := unique[tagged]; ok {
			continue
		}
		unique[tagged] = struct{}{}
		result = append(result, tagged)
	}
	return result, nil
}
