package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"

	"github.com/mbianchidev/porto/internal/nativefiles"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--probe" {
		if err := json.NewEncoder(os.Stdout).Encode(nativefiles.ProbeNativeDriver()); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: porto-files-mount [--probe]; mount requests arrive on the owned stdin channel")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := nativefiles.RunNativeMount(ctx, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "Porto native mount:", err)
		os.Exit(1)
	}
}
