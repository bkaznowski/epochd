package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/bkaznowski/epochd/pkg/api"
	"github.com/bkaznowski/epochd/pkg/sdk"
)

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

func addURLFlag(fs *flag.FlagSet) *string {
	return fs.String("url", "", "controller URL (overrides EPOCHD_URL env var)")
}

func clientFromURL(url string) (*sdk.Client, error) {
	if url == "" {
		url = os.Getenv("EPOCHD_URL")
	}
	if url == "" {
		return nil, fmt.Errorf("controller URL required: set EPOCHD_URL or pass --url")
	}
	return sdk.NewClient(url), nil
}

func newTabWriter() *tabwriter.Writer {
	return tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
}

// printTimeshift prints a single timeshift in multi-line key-value format.
// prefix is "created", "updated", "advanced", or "" (for get).
func printTimeshift(prefix string, ts *sdk.Timeshift) {
	if prefix != "" {
		fmt.Fprintf(stdout, "%s timeshift %s\n", prefix, ts.ID)
	} else {
		fmt.Fprintf(stdout, "timeshift %s\n", ts.ID)
	}
	fmt.Fprintf(stdout, "  namespace:  %s\n", ts.Namespace)
	fmt.Fprintf(stdout, "  time:       %s\n", ts.Time.Format(time.RFC3339))
	if ts.Frozen {
		fmt.Fprintf(stdout, "  frozen:     yes\n")
	}
	if !ts.ExpiresAt.IsZero() {
		fmt.Fprintf(stdout, "  expires at: %s\n", ts.ExpiresAt.Format(time.RFC3339))
	}
	applied := strings.Join(ts.AppliedTo, ", ")
	if applied == "" {
		applied = "-"
	}
	fmt.Fprintf(stdout, "  applied to: %s\n", applied)
}

// ---------------------------------------------------------------------------
// create
// ---------------------------------------------------------------------------

func cmdCreate(args []string) error {
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := addURLFlag(fs)
	ns := fs.String("namespace", "", "Kubernetes namespace (required)")
	sel := fs.String("selector", "", "label selector, e.g. app=web (required)")
	timeStr := fs.String("time", "", "fake time in RFC3339, e.g. 2030-01-01T00:00:00Z (required)")
	ttlStr := fs.String("ttl", "", "expiry duration, e.g. 1h (optional; omit for no expiry)")
	freeze := fs.Bool("freeze", false, "freeze clock at --time (clock does not advance past target)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *ns == "" {
		return fmt.Errorf("create: --namespace is required")
	}
	if *sel == "" {
		return fmt.Errorf("create: --selector is required")
	}
	if *timeStr == "" {
		return fmt.Errorf("create: --time is required")
	}
	target, err := time.Parse(time.RFC3339, *timeStr)
	if err != nil {
		return fmt.Errorf("create: --time: %v", err)
	}
	var ttl time.Duration
	if *ttlStr != "" {
		ttl, err = time.ParseDuration(*ttlStr)
		if err != nil || ttl <= 0 {
			return fmt.Errorf("create: --ttl must be a positive Go duration (e.g. 1h)")
		}
	}
	client, err := clientFromURL(*url)
	if err != nil {
		return err
	}
	var ts *sdk.Timeshift
	if *freeze {
		ts, err = client.CreateFrozenTimeshift(context.Background(), *ns, *sel, target, ttl)
	} else {
		ts, err = client.CreateTimeshift(context.Background(), *ns, *sel, target, ttl)
	}
	if err != nil {
		if sdk.IsConflict(err) {
			return fmt.Errorf("create: %v (use 'delete' to remove the existing timeshift first)", err)
		}
		return fmt.Errorf("create: %v", err)
	}
	printTimeshift("created", ts)
	return nil
}

// ---------------------------------------------------------------------------
// list
// ---------------------------------------------------------------------------

func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := addURLFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	client, err := clientFromURL(*url)
	if err != nil {
		return err
	}
	timeshifts, err := client.ListTimeshifts(context.Background())
	if err != nil {
		return fmt.Errorf("list: %v", err)
	}
	if len(timeshifts) == 0 {
		fmt.Fprintln(stdout, "no active timeshifts")
		return nil
	}
	w := newTabWriter()
	fmt.Fprintln(w, "ID\tNAMESPACE\tTARGET TIME\tMODE\tEXPIRES AT\tAPPLIED TO")
	for _, ts := range timeshifts {
		mode := "advancing"
		if ts.Frozen {
			mode = "frozen"
		}
		exp := "-"
		if !ts.ExpiresAt.IsZero() {
			exp = ts.ExpiresAt.Format(time.RFC3339)
		}
		applied := strings.Join(ts.AppliedTo, ", ")
		if applied == "" {
			applied = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			ts.ID, ts.Namespace, ts.Time.Format(time.RFC3339), mode, exp, applied)
	}
	w.Flush()
	return nil
}

// ---------------------------------------------------------------------------
// get
// ---------------------------------------------------------------------------

func cmdGet(args []string) error {
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := addURLFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("get: timeshift ID required")
	}
	client, err := clientFromURL(*url)
	if err != nil {
		return err
	}
	ts, err := client.GetTimeshift(context.Background(), fs.Arg(0))
	if err != nil {
		if sdk.IsNotFound(err) {
			return fmt.Errorf("get: timeshift %q not found", fs.Arg(0))
		}
		return fmt.Errorf("get: %v", err)
	}
	printTimeshift("", ts)
	return nil
}

// ---------------------------------------------------------------------------
// update
// ---------------------------------------------------------------------------

func cmdUpdate(args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := addURLFlag(fs)
	timeStr := fs.String("time", "", "new fake time in RFC3339 (required)")
	freeze := fs.Bool("freeze", false, "freeze clock at --time (pass false to thaw and advance)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("update: timeshift ID required")
	}
	if *timeStr == "" {
		return fmt.Errorf("update: --time is required")
	}
	target, err := time.Parse(time.RFC3339, *timeStr)
	if err != nil {
		return fmt.Errorf("update: --time: %v", err)
	}
	client, err := clientFromURL(*url)
	if err != nil {
		return err
	}
	var ts *sdk.Timeshift
	if *freeze {
		ts, err = client.FreezeTimeshift(context.Background(), fs.Arg(0), target)
	} else {
		ts, err = client.UpdateTimeshift(context.Background(), fs.Arg(0), target)
	}
	if err != nil {
		if sdk.IsNotFound(err) {
			return fmt.Errorf("update: timeshift %q not found", fs.Arg(0))
		}
		return fmt.Errorf("update: %v", err)
	}
	printTimeshift("updated", ts)
	return nil
}

// ---------------------------------------------------------------------------
// advance
// ---------------------------------------------------------------------------

func cmdAdvance(args []string) error {
	fs := flag.NewFlagSet("advance", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := addURLFlag(fs)
	byStr := fs.String("by", "", "duration to advance by, e.g. 24h or -1h (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("advance: timeshift ID required")
	}
	if *byStr == "" {
		return fmt.Errorf("advance: --by is required")
	}
	delta, err := time.ParseDuration(*byStr)
	if err != nil {
		return fmt.Errorf("advance: --by: %v", err)
	}
	client, err := clientFromURL(*url)
	if err != nil {
		return err
	}
	ts, err := client.AdvanceTimeshift(context.Background(), fs.Arg(0), delta)
	if err != nil {
		if sdk.IsNotFound(err) {
			return fmt.Errorf("advance: timeshift %q not found", fs.Arg(0))
		}
		return fmt.Errorf("advance: %v", err)
	}
	printTimeshift("advanced", ts)
	return nil
}

// ---------------------------------------------------------------------------
// delete
// ---------------------------------------------------------------------------

func cmdDelete(args []string) error {
	fs := flag.NewFlagSet("delete", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := addURLFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("delete: timeshift ID required")
	}
	id := fs.Arg(0)
	client, err := clientFromURL(*url)
	if err != nil {
		return err
	}
	if err := client.DeleteTimeshift(context.Background(), id); err != nil {
		if sdk.IsNotFound(err) {
			return fmt.Errorf("delete: timeshift %q not found", id)
		}
		return fmt.Errorf("delete: %v", err)
	}
	fmt.Fprintf(stdout, "deleted timeshift %s\n", id)
	return nil
}

// ---------------------------------------------------------------------------
// status
// ---------------------------------------------------------------------------

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := addURLFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("status: timeshift ID required")
	}
	id := fs.Arg(0)
	client, err := clientFromURL(*url)
	if err != nil {
		return err
	}
	resp, err := client.TimeshiftStatus(context.Background(), id)
	if err != nil {
		if sdk.IsNotFound(err) {
			return fmt.Errorf("status: timeshift %q not found", id)
		}
		return fmt.Errorf("status: %v", err)
	}
	printStatus(resp)
	return nil
}

func printStatus(resp *api.TimeshiftStatusResponse) {
	fmt.Fprintf(stdout, "timeshift %s  namespace: %s\n\n", resp.ID, resp.Namespace)
	w := newTabWriter()
	fmt.Fprintln(w, "POD\tCONTAINER\tGENERATION\tTARGET TIME\tPID\tERROR")
	for _, c := range resp.Containers {
		if c.Status != nil {
			fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%d\t%s\n",
				c.Pod, c.Container,
				c.Status.Generation, c.Status.LastTarget,
				c.Status.PID, c.Error)
		} else {
			fmt.Fprintf(w, "%s\t%s\t-\t-\t-\t%s\n",
				c.Pod, c.Container, c.Error)
		}
	}
	w.Flush()
}

// ---------------------------------------------------------------------------
// resolve
// ---------------------------------------------------------------------------

func cmdResolve(args []string) error {
	fs := flag.NewFlagSet("resolve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := addURLFlag(fs)
	ns := fs.String("namespace", "", "Kubernetes namespace (required)")
	sel := fs.String("selector", "", "label selector (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *ns == "" {
		return fmt.Errorf("resolve: --namespace is required")
	}
	if *sel == "" {
		return fmt.Errorf("resolve: --selector is required")
	}
	client, err := clientFromURL(*url)
	if err != nil {
		return err
	}
	pods, err := client.Resolve(context.Background(), *ns, *sel)
	if err != nil {
		return fmt.Errorf("resolve: %v", err)
	}
	if len(pods) == 0 {
		fmt.Fprintln(stdout, "no matching pods")
		return nil
	}
	w := newTabWriter()
	fmt.Fprintln(w, "POD\tNAMESPACE\tNODE IP\tCONTAINERS")
	for _, p := range pods {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			p.Name, p.Namespace, p.NodeIP, strings.Join(p.Containers, ", "))
	}
	w.Flush()
	return nil
}
