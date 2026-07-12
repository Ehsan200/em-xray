package main

import (
	"context"
	"fmt"
	"strings"

	emxv1 "github.com/ehsan200/em-xray/api/emxv1"
	"github.com/mdp/qrterminal/v3"
	"github.com/spf13/cobra"
)

// renderQR returns a terminal QR code (half-block, so it stays compact) for the
// given text, or an error message string if the text is empty.
func renderQR(text string) string {
	if text == "" {
		return "(no share link for this protocol)"
	}
	var b strings.Builder
	qrterminal.GenerateWithConfig(text, qrterminal.Config{
		Level:          qrterminal.M,
		Writer:         &b,
		HalfBlocks:     true,
		BlackChar:      qrterminal.BLACK_BLACK,
		WhiteChar:      qrterminal.WHITE_WHITE,
		BlackWhiteChar: qrterminal.BLACK_WHITE,
		WhiteBlackChar: qrterminal.WHITE_BLACK,
		QuietZone:      1,
	})
	return b.String()
}

func inboundQRCmd() *cobra.Command {
	return &cobra.Command{
		Use: "qr <id>", Short: "print the client share link as a scannable QR code",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID(args[0])
			if err != nil {
				return err
			}
			return withClient(cmd, func(ctx context.Context, cl emxv1.DaemonClient) error {
				reply, err := cl.InboundList(ctx, &emxv1.Empty{})
				if err != nil {
					return err
				}
				for _, in := range reply.Inbounds {
					if in.Id != id {
						continue
					}
					out := cmd.OutOrStdout()
					fmt.Fprintf(out, "%s\n\n%s\n%s\n", in.Name, renderQR(in.ShareLink), in.ShareLink)
					return nil
				}
				return fmt.Errorf("no inbound with id %d", id)
			})
		},
	}
}
