package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/hostedexport"
	"github.com/vadimtrunov/watchkeepers/core/pkg/localpatch"
	"github.com/vadimtrunov/watchkeepers/core/pkg/toolregistry"
)

// hostedExportFlags is the parsed flag bundle for
// `wk-tool hosted-export`.
type hostedExportFlags struct {
	source      string
	tool        string
	destination string
	reason      string
	operator    string
	dataDir     string
}

func runHostedExport(ctx context.Context, args []string, stdout, stderr io.Writer, env envLookup) int {
	fs := flag.NewFlagSet("wk-tool hosted-export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var f hostedExportFlags
	fs.StringVar(&f.source, "source", "", "name of the kind=hosted source to export from (required)")
	fs.StringVar(&f.tool, "tool", "", "name of the tool to export (required)")
	fs.StringVar(&f.destination, "destination", "", "absolute path of the operator-supplied destination directory (required; absent or empty)")
	fs.StringVar(&f.reason, "reason", "", "operator-supplied audit text (required)")
	fs.StringVar(&f.operator, "operator", "", "operator identity (required)")
	fs.StringVar(&f.dataDir, "data-dir", "", "deployment data directory; falls back to $"+dataDirEnvKey)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if err := validateHostedExportFlags(&f, env); err != nil {
		stderrf(stderr, "wk-tool: hosted-export: "+err.Error()+"\n")
		return 2
	}
	if err := hostedexport.ValidateOperatorID(f.operator); err != nil {
		stderrf(stderr, "wk-tool: hosted-export: invalid --operator\n")
		return 2
	}
	exporter := buildExporter(f, stdout)
	res, err := exporter.Export(ctx, hostedexport.ExportRequest{
		SourceName:     f.source,
		ToolName:       f.tool,
		Destination:    f.destination,
		Reason:         f.reason,
		OperatorIDHint: f.operator,
	})
	if err != nil {
		stderrf(stderr, fmt.Sprintf("wk-tool: hosted-export: %v\n", err))
		return 1
	}
	fmt.Fprintf(
		stdout, "wk-tool: hosted-export ok source=%s tool=%s version=%s bundle_digest=%s correlation_id=%s\n",
		f.source, f.tool, res.ToolVersion, res.BundleDigest, res.CorrelationID,
	)
	return 0
}

func validateHostedExportFlags(f *hostedExportFlags, env envLookup) error {
	missing := []string{}
	if strings.TrimSpace(f.source) == "" {
		missing = append(missing, "source")
	}
	if strings.TrimSpace(f.tool) == "" {
		missing = append(missing, "tool")
	}
	if strings.TrimSpace(f.destination) == "" {
		missing = append(missing, "destination")
	}
	if strings.TrimSpace(f.reason) == "" {
		missing = append(missing, "reason")
	}
	if strings.TrimSpace(f.operator) == "" {
		missing = append(missing, "operator")
	}
	if f.dataDir == "" {
		if v, ok := env.LookupEnv(dataDirEnvKey); ok && strings.TrimSpace(v) != "" {
			f.dataDir = v
		} else {
			missing = append(missing, "data-dir (or "+dataDirEnvKey+")")
		}
	}
	if len(missing) > 0 {
		return errMissingFlags{names: missing}
	}
	return nil
}

func buildExporter(f hostedExportFlags, stdout io.Writer) *hostedexport.Exporter {
	clk := localpatch.ClockFunc(time.Now)
	pub := newJSONLPublisher(stdout, clk)
	return hostedexport.NewExporter(hostedexport.ExporterDeps{
		FS:                       hostedexport.OSFS{},
		Publisher:                pub,
		Clock:                    clk,
		SourceLookup:             hostedSourceLookup(f.source),
		OperatorIdentityResolver: trustHintResolver,
		DataDir:                  f.dataDir,
	})
}

// hostedSourceLookup returns a [hostedexport.SourceLookup] that
// resolves the operator-supplied `--source <name>` to a
// `kind: hosted` config. Mirror [singleSourceLookup]'s scope: the
// CLI is a one-shot operator tool and trusts the operator's flag.
// Production deployments swap this for a real config-backed lookup.
func hostedSourceLookup(name string) hostedexport.SourceLookup {
	return func(_ context.Context, requested string) (toolregistry.SourceConfig, error) {
		if requested != name {
			return toolregistry.SourceConfig{}, fmt.Errorf("source %q not configured (cli is scoped to %q)", requested, name)
		}
		return toolregistry.SourceConfig{
			Name:       name,
			Kind:       toolregistry.SourceKindHosted,
			PullPolicy: toolregistry.PullPolicyOnDemand,
		}, nil
	}
}
