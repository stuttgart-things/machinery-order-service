// Package cmd holds the CLI: serve (default) and version.
package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/stuttgart-things/machinery-order-service/internal/version"
)

var root = &cobra.Command{
	Use:   "machinery-order-service",
	Short: "Orders for machinery: Order CRs, render through claim-machinery-api, delivery as git pull requests",
	RunE:  func(c *cobra.Command, args []string) error { return serve(c.Context()) },
}

func init() {
	root.AddCommand(&cobra.Command{
		Use:   "serve",
		Short: "Run the controller and the API (default)",
		RunE:  func(c *cobra.Command, args []string) error { return serve(c.Context()) },
	})
	root.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Run: func(*cobra.Command, []string) {
			i := version.Info()
			fmt.Printf("machinery-order-service %s (commit %s, built %s)\n", i["version"], i["commit"], i["buildDate"])
		},
	})
}

// Execute runs the CLI.
func Execute() {
	if err := root.ExecuteContext(ctrl.SetupSignalHandler()); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
