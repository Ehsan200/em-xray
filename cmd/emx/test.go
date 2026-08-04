package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	emxv1 "github.com/ehsan200/em-xray/api/emxv1"
	"github.com/spf13/cobra"
)

// testTimeout bounds the whole RPC: a big subscription probes in batches, and
// each batch pays an xray start-up before its (concurrent) probes.
const testTimeout = 5 * time.Minute

// entryTestCmd measures the real latency through one entry's outbound (or every
// entry when no id is given).
func entryTestCmd() *cobra.Command {
	var timeout int
	var url string
	c := &cobra.Command{
		Use: "test [id]", Short: "measure real latency through an entry (all entries if omitted)",
		Aliases: []string{"ping"}, Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req := &emxv1.TestRequest{Kind: "entries", TimeoutSec: int32(timeout), Url: url}
			if len(args) == 1 {
				id, err := parseID(args[0])
				if err != nil {
					return err
				}
				req.Kind, req.Id = "entry", id
			}
			return runTest(cmd, req, false)
		},
	}
	c.Flags().IntVar(&timeout, "timeout", 0, "per-config probe timeout in seconds (0 = 10)")
	c.Flags().StringVar(&url, "url", "", "probe URL (default http://www.gstatic.com/generate_204)")
	return c
}

// subTestCmd measures every node of a subscription (or one node by fingerprint)
// and persists the results, so `sub nodes` and the TUI show them afterwards.
func subTestCmd() *cobra.Command {
	var timeout int
	var url string
	c := &cobra.Command{
		Use: "test <id> [fingerprint]", Short: "measure real latency through a subscription's nodes",
		Aliases: []string{"ping"}, Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID(args[0])
			if err != nil {
				return err
			}
			req := &emxv1.TestRequest{Kind: "sub", Id: id, TimeoutSec: int32(timeout), Url: url}
			if len(args) == 2 {
				req.Kind, req.Fingerprint = "node", args[1]
			}
			return runTest(cmd, req, true)
		},
	}
	c.Flags().IntVar(&timeout, "timeout", 0, "per-config probe timeout in seconds (0 = 10)")
	c.Flags().StringVar(&url, "url", "", "probe URL (default http://www.gstatic.com/generate_204)")
	return c
}

// runTest issues the Test RPC and prints the table. withFingerprint adds the
// fingerprint column (subscription nodes), whose ids are meaningless.
func runTest(cmd *cobra.Command, req *emxv1.TestRequest, withFingerprint bool) error {
	fmt.Fprintln(cmd.ErrOrStderr(), "testing… (each config gets its own throwaway xray)")
	return withClientTimeout(cmd, testTimeout, func(ctx context.Context, cl emxv1.DaemonClient) error {
		reply, err := cl.Test(ctx, req)
		if err != nil {
			return err
		}
		if len(reply.Results) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "nothing to test")
			return nil
		}
		printTestResults(cmd.OutOrStdout(), reply.Results, withFingerprint)
		return nil
	})
}

// printTestResults renders results fastest-first, failures last.
func printTestResults(w io.Writer, results []*emxv1.TestResult, withFingerprint bool) {
	sorted := append([]*emxv1.TestResult(nil), results...)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if (a.Error == "") != (b.Error == "") {
			return a.Error == "" // successes first
		}
		if a.Error != "" {
			return false // keep input order among failures
		}
		return a.LatencyMs < b.LatencyMs
	})

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	head := "NAME\tLATENCY\tRESULT"
	if withFingerprint {
		head = "FINGERPRINT\t" + head
	} else {
		head = "ID\t" + head
	}
	fmt.Fprintln(tw, head)
	ok := 0
	for _, r := range sorted {
		lead := fmt.Sprintf("%d", r.Id)
		if withFingerprint {
			lead = r.Fingerprint
		}
		if r.Error == "" {
			ok++
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", lead, r.Name, latencyText(r.LatencyMs), "ok")
		} else {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", lead, r.Name, "-", "FAIL: "+r.Error)
		}
	}
	tw.Flush()
	fmt.Fprintf(w, "\n%d/%d reachable\n", ok, len(sorted))
}

// latencyText renders a measured round-trip, "-" when unknown.
func latencyText(ms int32) string {
	if ms <= 0 {
		return "-"
	}
	return fmt.Sprintf("%dms", ms)
}

// testSummary is the one-line form the TUI shows after a test run.
func testSummary(results []*emxv1.TestResult) string {
	if len(results) == 0 {
		return "nothing to test"
	}
	if len(results) == 1 {
		r := results[0]
		if r.Error != "" {
			return fmt.Sprintf("%s: FAIL — %s", r.Name, r.Error)
		}
		return fmt.Sprintf("%s: %s", r.Name, latencyText(r.LatencyMs))
	}
	ok, best, bestName := 0, int32(0), ""
	for _, r := range results {
		if r.Error != "" {
			continue
		}
		ok++
		if best == 0 || r.LatencyMs < best {
			best, bestName = r.LatencyMs, r.Name
		}
	}
	if ok == 0 {
		return fmt.Sprintf("0/%d reachable", len(results))
	}
	return fmt.Sprintf("%d/%d reachable · fastest %s (%s)", ok, len(results), bestName, latencyText(best))
}

// testResultLines renders the full result table for the TUI, fastest first.
func testResultLines(results []*emxv1.TestResult, withFingerprint bool) string {
	var b strings.Builder
	printTestResults(&b, results, withFingerprint)
	return b.String()
}
