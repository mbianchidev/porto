package main

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"
)

func runTTYFixture() {
	fmt.Println("porto-native-tty-ready")
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		command := strings.Fields(scanner.Text())
		if len(command) == 0 {
			continue
		}
		switch command[0] {
		case "size":
			w, h, err := term.GetSize(int(os.Stdout.Fd()))
			if err != nil {
				panic(err)
			}
			fmt.Printf("porto-native-tty-size:%dx%d\n", w, h)
		case "wait-size":
			if len(command) != 3 {
				panic("wait-size requires width and height")
			}
			width, _ := strconv.Atoi(command[1])
			height, _ := strconv.Atoi(command[2])
			until := time.Now().Add(5 * time.Second)
			for {
				w, h, err := term.GetSize(int(os.Stdout.Fd()))
				if err != nil {
					panic(err)
				}
				if w == width && h == height {
					fmt.Printf("porto-native-tty-size:%dx%d\n", w, h)
					break
				}
				if time.Now().After(until) {
					panic(fmt.Sprintf("TTY resize remained %dx%d instead of %dx%d", w, h, width, height))
				}
				time.Sleep(25 * time.Millisecond)
			}
		case "quit":
			fmt.Println("porto-native-tty-final-output")
			os.Exit(37)
		default:
			panic("unknown TTY fixture command")
		}
	}
	if err := scanner.Err(); err != nil {
		panic(err)
	}
}

func main() {
	root := `C:\fixture`
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "tty":
			runTTYFixture()
			return
		case "exec":
			if err := os.WriteFile(filepath.Join(root, "exec.txt"), []byte("native-exec-write"), 0o600); err != nil {
				panic(err)
			}
			content, err := os.ReadFile(filepath.Join(root, "host.txt"))
			if err != nil {
				panic(err)
			}

			fmt.Println(string(content))
			fmt.Fprintln(os.Stderr, "porto-native-exec-stderr")
			return
		case "fail":
			os.Exit(37)
		default:
			panic("unknown native acceptance fixture action")
		}
	}
	if err := os.WriteFile(filepath.Join(root, "container.txt"), []byte("native-to-host-one"), 0o600); err != nil {
		panic(err)
	}
	server := &http.Server{Addr: ":8180", ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			content, err := os.ReadFile(filepath.Join(root, "host.txt"))
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Connection", "close")
			_, _ = w.Write(content)
		})}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		stopContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(stopContext)
	}()
	fmt.Println("porto-native-ready")
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		panic(err)
	}
}
