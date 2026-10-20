//go:build laptop

package bus

import (
	"context"
	"errors"
)

// The laptop binary is built with the laptop tag: murmur solo's hub is one instance, on the local bus.
var errLaptop = errors.New("the laptop binary's hub is one instance: run the murmur-hub binary or image for this bus")

func openPostgres(context.Context, string) (Bus, error) { return nil, errLaptop }
func openRedis(context.Context, string) (Bus, error)    { return nil, errLaptop }
