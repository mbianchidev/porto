//go:build !linux

package main

import (
	"context"
	"errors"
)

var errLinuxRequired = errors.New("Porto runtime helper CNI operations require Linux")

func probeRuntime() runtimeProbe {
	return runtimeProbe{
		CNIReason:  errLinuxRequired.Error(),
		CRIUReason: errLinuxRequired.Error(),
	}
}

func connectCNI(context.Context, cniRequest) (any, error) {
	return nil, errLinuxRequired
}

func disconnectCNI(context.Context, cniRequest) error {
	return errLinuxRequired
}
