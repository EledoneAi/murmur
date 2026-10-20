//go:build laptop

package store

import (
	"context"
	"errors"
)

// The laptop binary is built with the laptop tag. It runs a hub only for murmur solo, in memory, so it leaves out
// the stores' clients, which would more than double its size.
var errLaptop = errors.New("the laptop binary keeps its hub in memory: run the murmur-hub binary or image for this store")

func openSQLite(context.Context, string) (Store, error)            { return nil, errLaptop }
func openPostgres(context.Context, string) (Store, error)          { return nil, errLaptop }
func openFirestore(context.Context, string, string) (Store, error) { return nil, errLaptop }
