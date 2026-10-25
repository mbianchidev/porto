//go:build !windows

package networking

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

func systemProxy() (Config, string, error) {
	if configuration, configured := environmentProxy(); configured || runtime.GOOS != "darwin" {
		return configuration, "environment", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "/usr/sbin/scutil", "--proxy").Output()
	if err != nil {
		return Config{}, "", fmt.Errorf("read macOS proxy settings; configure manual proxies or disable proxy use explicitly: %w", err)
	}
	configuration, err := parseMacProxy(string(output))
	return configuration, "macOS static system proxy", err
}

func parseMacProxy(output string) (Config, error) {
	values := map[string]string{}
	var bypass []string
	inExceptions := false
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "ExceptionsList") {
			inExceptions = true
			continue
		}
		if inExceptions && line == "}" {
			inExceptions = false
			continue
		}
		key, value, found := strings.Cut(line, " : ")
		if !found {
			continue
		}
		if inExceptions {
			bypass = append(bypass, value)
		} else {
			values[key] = value
		}
	}
	if values["ProxyAutoConfigEnable"] == "1" || values["ProxyAutoDiscoveryEnable"] == "1" {
		return Config{}, errorsPAC()
	}
	configuration := Config{Mode: "system", NoProxy: strings.Join(bypass, ",")}
	for name, destination := range map[string]*string{"HTTP": &configuration.HTTPProxy, "HTTPS": &configuration.HTTPSProxy, "SOCKS": &configuration.SOCKSProxy} {
		if values[name+"Enable"] == "1" {
			scheme := "http"
			if name == "SOCKS" {
				scheme = "socks5"
			}
			host, port := values[name+"Proxy"], values[name+"Port"]
			if host == "" || port == "" {
				return Config{}, fmt.Errorf("macOS %s proxy is enabled without a host or port", name)
			}
			*destination = scheme + "://" + net.JoinHostPort(host, port)
		}
	}
	return configuration, nil
}
