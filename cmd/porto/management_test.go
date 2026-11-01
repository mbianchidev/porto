package main

import "testing"

func TestManagementCommandsRequireExplicitTargetsAndConsent(t *testing.T) {
	for _, args := range [][]string{
		{"environments"}, {"environments", "request"}, {"environments", "remove", "synthetic"},
		{"environments", "revoke", "synthetic"}, {"environments", "stop-listener"},
		{"environments", "fleet-run", "synthetic"}, {"environments", "listen"},
		{"extension"}, {"extension", "install", "synthetic.porto-extension"},
		{"extension", "enable", "synthetic", "grants.json"}, {"extension", "remove", "synthetic"},
		{"extension", "invoke", "synthetic", "inspect"}, {"extension", "pack"}, {"extension", "keygen"},
	} {
		t.Run(args[0]+"/"+args[len(args)-1], func(t *testing.T) {
			var err error
			switch args[0] {
			case "environments":
				err = environmentsCmd(args[1:])
			case "extension":
				err = extensionCmd(args[1:])
			}
			if err == nil {
				t.Fatalf("unsafe or incomplete management command accepted: %v", args)
			}
		})
	}
}
