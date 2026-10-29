package delivery

import "strings"

func reservedVariable(name string) bool {
	name = strings.ToUpper(name)
	for _, prefix := range []string{"DOCKER_", "COMPOSE_", "PORTO_", "GIT_", "LD_", "DYLD_"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	switch name {
	case "PATH", "HOME", "USERPROFILE", "SYSTEMROOT", "TMP", "TEMP", "TMPDIR", "BASH_ENV", "ENV", "SHELLOPTS", "NODE_OPTIONS", "GODEBUG", "GOMAXPROCS", "GOMEMLIMIT", "SHELL", "COMSPEC", "PATHEXT", "APPDATA", "LOCALAPPDATA":
		return true
	}
	return false
}
