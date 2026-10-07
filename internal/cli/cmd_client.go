package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"regexp"
	"text/tabwriter"

	"github.com/jamshids/rillock/internal/apiv1"
	"github.com/jamshids/rillock/internal/client"
	"github.com/jamshids/rillock/internal/config"
)

// parseClient parses a client command's flags and returns a connected client
// and the positional arguments.
func parseClient(a *app, fs *flag.FlagSet, args []string) (*client.Client, []string, error) {
	var c config.Client
	rest, err := config.Parse(fs, args, a.env, &c)
	if err != nil {
		return nil, nil, err
	}
	cl, err := client.New(c.Server, c.APIKey)
	if err != nil {
		return nil, nil, err
	}
	return cl, rest, nil
}

func runApply(ctx context.Context, a *app, fs *flag.FlagSet, args []string) error {
	var files stringsFlag
	fs.Var(&files, "f", "YAML file with resources (repeatable)")
	cl, rest, err := parseClient(a, fs, args)
	if err != nil {
		return err
	}
	if err := noArgs(rest); err != nil {
		return err
	}
	if len(files) == 0 {
		return usageErrorf("at least one -f <file> is required")
	}

	resources, err := readManifests(files)
	if err != nil {
		return err
	}
	resp, err := cl.Apply(ctx, resources)
	if err != nil {
		return err
	}
	for _, r := range resp.Results {
		fmt.Fprintf(a.stdout, "%s/%s %s (version %d)\n", lowerKind(r.Kind), r.Name, r.Status, r.Version)
	}
	return nil
}

func runRun(ctx context.Context, a *app, fs *flag.FlagSet, args []string) error {
	text := fs.String("input", "", "input for the agent, as plain text")
	rawJSON := fs.String("input-json", "", "input for the agent, as a JSON value")
	cl, rest, err := parseClient(a, fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usageErrorf("expected exactly one agent name")
	}

	var input json.RawMessage
	switch {
	case *text != "" && *rawJSON != "":
		return usageErrorf("use --input or --input-json, not both")
	case *text != "":
		input, err = json.Marshal(*text) // plain text becomes a JSON string
		if err != nil {
			return err
		}
	case *rawJSON != "":
		if !json.Valid([]byte(*rawJSON)) {
			return usageErrorf("--input-json is not valid JSON")
		}
		input = json.RawMessage(*rawJSON)
	}

	run, err := cl.SubmitRun(ctx, rest[0], input)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "run/%s submitted (agent %s, version %d)\n", run.ID, run.Agent, run.AgentVersion)
	return nil
}

func runGet(ctx context.Context, a *app, fs *flag.FlagSet, args []string) error {
	state := fs.String("state", "", "only runs in this state (for get runs)")
	limit := fs.Int("limit", 0, "maximum number of runs to show (for get runs; default 50)")
	cl, rest, err := parseClient(a, fs, args)
	if err != nil {
		return err
	}

	switch {
	case len(rest) == 1 && rest[0] == "runs":
		runs, err := cl.ListRuns(ctx, client.ListRunsOptions{State: *state, Limit: *limit})
		if err != nil {
			return err
		}
		a.printRuns(runs)
		return nil
	case len(rest) == 2 && rest[0] == "run":
		id, err := resolveRunID(ctx, cl, rest[1])
		if err != nil {
			return err
		}
		run, err := cl.GetRun(ctx, id)
		if err != nil {
			return err
		}
		events, err := cl.ListEvents(ctx, id, 0)
		if err != nil {
			return err
		}
		a.printRun(run, events)
		return nil
	default:
		return usageErrorf("expected `get runs` or `get run <id>`")
	}
}

func runCancel(ctx context.Context, a *app, fs *flag.FlagSet, args []string) error {
	cl, rest, err := parseClient(a, fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usageErrorf("expected exactly one run ID")
	}
	id, err := resolveRunID(ctx, cl, rest[0])
	if err != nil {
		return err
	}
	run, err := cl.CancelRun(ctx, id)
	if err != nil {
		return err
	}
	if run.State == "cancelled" {
		fmt.Fprintf(a.stdout, "run/%s cancelled\n", run.ID)
	} else {
		fmt.Fprintf(a.stdout, "run/%s cancel requested: its worker stops it after the current step\n", run.ID)
	}
	return nil
}

// fullRunID matches a complete run ID.
var fullRunID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// resolveRunID turns what the user typed into a full run ID. A full ID is used
// as it is. A shorter one is looked up on the server, which sees every run
// (not only the newest page), and must match exactly one run.
func resolveRunID(ctx context.Context, cl *client.Client, typed string) (string, error) {
	if fullRunID.MatchString(typed) {
		return typed, nil
	}
	// Two results are enough to tell "exactly one" from "more than one".
	runs, err := cl.ListRuns(ctx, client.ListRunsOptions{IDPrefix: typed, Limit: 2})
	if err != nil {
		return "", err
	}
	switch len(runs) {
	case 0:
		return "", fmt.Errorf("no run ID starts with %q", typed)
	case 1:
		return runs[0].ID, nil
	default:
		// Run IDs begin with a timestamp, so runs created close together
		// share their first characters.
		return "", fmt.Errorf("more than one run ID starts with %q (for example %s and %s); type more of the ID",
			typed, runs[0].ID, runs[1].ID)
	}
}

func (a *app) printRuns(runs []apiv1.Run) {
	if len(runs) == 0 {
		fmt.Fprintln(a.stdout, "no runs")
		return
	}
	tw := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RUN\tAGENT\tSTATE\tSTEPS\tCOST\tAGE")
	for _, r := range runs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\n",
			r.ID, r.Agent, r.State, r.StepsUsed, formatCost(r.CostMicroUSD), formatAge(a.now().Sub(r.CreatedAt)))
	}
	_ = tw.Flush()
}

func (a *app) printRun(r apiv1.Run, events []apiv1.Event) {
	tw := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "Run:\t%s\n", r.ID)
	fmt.Fprintf(tw, "Agent:\t%s (version %d)\n", r.Agent, r.AgentVersion)
	fmt.Fprintf(tw, "State:\t%s\n", r.State)
	if r.CancelRequested && r.State == "running" {
		fmt.Fprintf(tw, "\tcancel requested\n")
	}
	if r.FailureReason != "" {
		fmt.Fprintf(tw, "Failure:\t%s\n", r.FailureReason)
	}
	fmt.Fprintf(tw, "Input:\t%s\n", r.Input)
	if len(r.Result) > 0 {
		fmt.Fprintf(tw, "Result:\t%s\n", r.Result)
	}
	fmt.Fprintf(tw, "Usage:\t%d steps, %d input + %d output tokens, %s\n",
		r.StepsUsed, r.InputTokens, r.OutputTokens, formatCost(r.CostMicroUSD))
	fmt.Fprintf(tw, "Created:\t%s (%s ago)\n", r.CreatedAt.Local().Format("2006-01-02 15:04:05"), formatAge(a.now().Sub(r.CreatedAt)))
	_ = tw.Flush()

	fmt.Fprintln(a.stdout, "\nEvents:")
	tw = tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
	for _, e := range events {
		fmt.Fprintf(tw, "  %d\t%s\t%s\t%s\n", e.Seq, e.CreatedAt.Local().Format("15:04:05"), e.Type, compactJSON(e.Payload))
	}
	_ = tw.Flush()
}
