//go:build unix

package budget

import (
	"context"

	"github.com/core-slot/coreslot"
)

// client is the part of *coreslot.Client the gate uses; realClient is a thin wrapper.
type client interface {
	Enqueue(ctx context.Context, r coreslot.Request) (waiter, error)
	Adopt(ctx context.Context, r coreslot.Request) (*coreslot.Grant, error)
	Snapshot() (coreslot.Report, error)
}

// waiter is the part of *coreslot.Waiter the gate uses.
type waiter interface {
	Wait(ctx context.Context) (*coreslot.Grant, error)
}

type realClient struct{ c *coreslot.Client }

// openReal opens coreslot's budget as a client.
func openReal(o coreslot.Options) (client, error) {
	c, err := coreslot.Open(o)
	if err != nil {
		return nil, err
	}
	return realClient{c}, nil
}

func (r realClient) Enqueue(ctx context.Context, req coreslot.Request) (waiter, error) {
	w, err := r.c.Enqueue(ctx, req)
	if err != nil {
		return nil, err // never a typed nil inside the interface
	}
	return w, nil
}

func (r realClient) Adopt(ctx context.Context, req coreslot.Request) (*coreslot.Grant, error) {
	return r.c.Adopt(ctx, req)
}

func (r realClient) Snapshot() (coreslot.Report, error) { return r.c.Snapshot() }
