package cmd

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"agent-platform/internal/cli/output"
	harnesspb "agent-platform/pkg/pb/harnessloop"
)

func newLogsCmd() *cobra.Command {
	var (
		task      string
		stage     string
		eventType string
		sinceSeq  int64
		follow    bool
		limit     int32
	)
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Show task events (optionally follow)",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, conn, tenant, err := newClient(cmd)
			if err != nil {
				return err
			}
			defer conn.Close()
			ctx := callContext(cmd, tenant)

			if limit <= 0 {
				limit = 100
			}
			for {
				resp, listErr := client.ListEvents(ctx, &harnesspb.ListEventsRequest{
					TenantId: tenant, TaskId: task, Stage: stage, EventType: eventType,
					SinceSeq: sinceSeq, Limit: limit,
				})
				if listErr != nil {
					emit(cmd, describeError(listErr))
					return &ExitError{Code: exitCodeForError(listErr)}
				}
				for _, e := range resp.Events {
					line, _ := json.Marshal(map[string]any{
						"seq":       e.GetSeq(),
						"type":      e.GetEventType(),
						"stage":     e.GetStage(),
						"attempt":   e.GetAttempt(),
						"severity":  e.GetSeverity(),
						"timestamp": e.GetTimestampMs(),
						"payload":   string(e.GetPayload()),
					})
					fmt.Fprintln(cmd.OutOrStdout(), output.Redact(string(line)))
				}
				if !follow {
					return nil
				}
				if resp.NextSinceSeq > sinceSeq {
					sinceSeq = resp.NextSinceSeq
				}
				select {
				case <-cmd.Context().Done():
					return nil
				case <-time.After(time.Second):
				}
			}
		},
	}
	cmd.Flags().StringVar(&task, "task", "", "task id")
	cmd.Flags().StringVar(&stage, "stage", "", "filter by stage")
	cmd.Flags().StringVar(&eventType, "type", "", "filter by event type")
	cmd.Flags().Int64Var(&sinceSeq, "since", 0, "only events with seq greater than this")
	cmd.Flags().BoolVar(&follow, "follow", false, "follow new events")
	cmd.Flags().Int32Var(&limit, "limit", 100, "max events per poll")
	return cmd
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print CLI version",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), "harness-cli 0.1.0-p0")
			return nil
		},
	}
}
