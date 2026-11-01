package daemon

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/mbianchidev/porto/internal/runtimes"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	kubeclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

func bridgeImportedPodTerminal(w http.ResponseWriter, r *http.Request, config *rest.Config, client *kubeclient.Clientset, namespace, pod, container, shell string) {
	request := client.CoreV1().RESTClient().Post().Namespace(namespace).Resource("pods").Name(pod).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{Container: container, Command: podTerminalShellCommand(shell), Stdin: true, Stdout: true, TTY: true}, scheme.ParameterCodec)
	bridgeProcessTerminal(w, r, func(ctx context.Context) (runtimes.Process, error) {
		primary, err := remotecommand.NewWebSocketExecutor(config, "GET", request.URL().String())
		if err != nil {
			return nil, errors.New("selected Kubernetes WebSocket terminal transport could not be configured")
		}
		secondary, err := remotecommand.NewSPDYExecutor(config, "POST", request.URL())
		if err != nil {
			return nil, errors.New("selected Kubernetes SPDY terminal transport could not be configured")
		}
		executor, err := remotecommand.NewFallbackExecutor(primary, secondary, func(err error) bool { return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err) })
		if err != nil {
			return nil, err
		}
		return startKubernetesRemoteProcess(ctx, executor), nil
	}, "selected Kubernetes terminal failed")
}

type kubernetesRemoteProcess struct {
	ctx         context.Context
	cancel      context.CancelFunc
	input       *io.PipeWriter
	inputReader *io.PipeReader
	output      *io.PipeReader
	sizes       chan remotecommand.TerminalSize
	done        chan struct{}
	err         error
}

func startKubernetesRemoteProcess(parent context.Context, executor remotecommand.Executor) *kubernetesRemoteProcess {
	ctx, cancel := context.WithCancel(parent)
	inputReader, input := io.Pipe()
	output, writer := io.Pipe()
	process := &kubernetesRemoteProcess{ctx: ctx, cancel: cancel, input: input, inputReader: inputReader, output: output, sizes: make(chan remotecommand.TerminalSize, 1), done: make(chan struct{})}
	process.sizes <- remotecommand.TerminalSize{Width: 120, Height: 32}
	go func() {
		defer close(process.done)
		defer cancel()
		err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdin: inputReader, Stdout: writer, Tty: true, TerminalSizeQueue: process})
		if ctx.Err() != nil {
			err = ctx.Err()
		} else if err != nil {
			var exit interface{ ExitCode() int }
			if errors.As(err, &exit) {
				err = kubernetesTerminalExit(exit.ExitCode())
			} else {
				err = errors.New("the selected Kubernetes exec stream failed; verify this identity's exec permission and connectivity")
			}
		}
		process.err = err
		inputReader.CloseWithError(err)
		writer.CloseWithError(err)
	}()
	return process
}

type kubernetesTerminalExit int

func (e kubernetesTerminalExit) Error() string           { return "selected Kubernetes terminal exited" }
func (e kubernetesTerminalExit) ExitCode() int           { return int(e) }
func (p *kubernetesRemoteProcess) Stdin() io.WriteCloser { return p.input }
func (p *kubernetesRemoteProcess) Stdout() io.ReadCloser { return p.output }
func (p *kubernetesRemoteProcess) Stderr() io.ReadCloser { return io.NopCloser(strings.NewReader("")) }
func (p *kubernetesRemoteProcess) PID() int              { return 0 }
func (p *kubernetesRemoteProcess) Wait() error           { <-p.done; return p.err }
func (p *kubernetesRemoteProcess) Kill() error {
	p.cancel()
	return errors.Join(p.inputReader.CloseWithError(context.Canceled), p.output.CloseWithError(context.Canceled))
}
func (p *kubernetesRemoteProcess) Resize(ctx context.Context, width, height uint32) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.ctx.Err(); err != nil {
		return err
	}
	if width < 1 || width > 1000 || height < 1 || height > 1000 {
		return errors.New("terminal dimensions must be 1-1000")
	}
	select {
	case <-p.sizes:
	default:
	}
	select {
	case p.sizes <- remotecommand.TerminalSize{Width: uint16(width), Height: uint16(height)}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-p.ctx.Done():
		return p.ctx.Err()
	}
}
func (p *kubernetesRemoteProcess) Next() *remotecommand.TerminalSize {
	if p.ctx.Err() != nil {
		return nil
	}
	select {
	case size := <-p.sizes:
		return &size
	case <-p.ctx.Done():
		return nil
	}
}
