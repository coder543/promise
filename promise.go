package main

import (
	"context"

	"fmt"
	"reflect"
	"time"
)

var pglobal *Promise[string]

func main() {
	pglobal = other()
}

func other() *Promise[string] {
	p1 := SpawnWithExpiration(context.Background(), 3*time.Second, func(ctx context.Context) (string, error) {
		return "hi", nil
	})

	p2 := SpawnWithExpiration(context.Background(), 3*time.Second, func(ctx context.Context) (string, error) {
		return "hello", nil
	})

	x, _ := AwaitFirst(p1, p2)
	if x == "hi" {
		panic("")
	}

	return p1
}

// PromiseFunc is a function that will compute a promised value
type PromiseFunc[T any] func(ctx context.Context) (T, error)

// Promise represents a value that may not yet exist.
// The task to compute the value may return the value or an error.
// If the task panicks, it will be caught and treated as an error.
type Promise[T any] struct {
	cancel context.CancelFunc
	status chan struct{}

	value T
	err   error
}

// Await will block until this Promise returns either the value or an error.
// A Promise may be safely awaited concurrently or multiple times, and the
// same value will be returned to all waiters.
func (p *Promise[T]) Await() (T, error) {
	<-p.status
	return p.value, p.err
}

// Done provides a channel that closes when the Promise task has finished.
func (p *Promise[T]) Done() <-chan struct{} {
	return p.status
}

// Cancel will signal the cancellation of this promise's context
func (p *Promise[T]) Cancel() {
	p.cancel()
}

// Spawn will start the given task with a new, cancellable context derived from ctx.
func Spawn[T any](ctx context.Context, f PromiseFunc[T]) *Promise[T] {
	ctx, cancel := context.WithCancel(ctx)
	return spawn(ctx, cancel, f)
}

// SpawnWithExpiration will start the given task with a new, cancellable context derived from ctx
// with an added expiration time to limit the duration of task's context.
func SpawnWithExpiration[
	T any,
	TimeOrDuration interface{ time.Time | time.Duration },
](
	ctx context.Context,
	expiration TimeOrDuration,
	f PromiseFunc[T],
) *Promise[T] {

	var cancel context.CancelFunc

	switch exp := (interface{})(expiration).(type) {
	case time.Duration:
		ctx, cancel = context.WithTimeout(ctx, exp)
	case time.Time:
		ctx, cancel = context.WithDeadline(ctx, exp)

	default:
		// If only Go generics supported exhaustive matching for this case
		panic("unreachable")
	}

	return spawn(ctx, cancel, f)
}

// spawn provides the underlying implementation of Spawn and SpawnWithExpiration
func spawn[T any](ctx context.Context, cancel context.CancelFunc, f PromiseFunc[T]) *Promise[T] {
	p := &Promise[T]{cancel: cancel, status: make(chan struct{})}

	go func() {
		defer close(p.status)
		defer cancel()
		defer func() {
			if r := recover(); r != nil {
				p.err = fmt.Errorf("promise panicked: %v", r)
			}
		}()

		p.value, p.err = f(ctx)
	}()

	return p
}

var closedChannel = func() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}()

// WithValue will construct a prefilled Promise containing the given value.
// Outside of rare cases, you probably don't want this.
// An example of where this is useful is a function that always returns a
// Promise, but certain guard conditions allow it to synchronously provide
// the requested value without an expensive background computation.
func WithValue[T any](value T) *Promise[T] {
	return &Promise[T]{cancel: func() {}, status: closedChannel, value: value}
}

// WithError will construct a prefilled Promise containing the given error.
// Outside of rare cases, you probably don't want this.
// An example of where this is useful is a function that always returns a
// Promise, but certain guard conditions allow it to synchronously know
// the background task would fail, so it can provide an immediate error.
func WithError[T any](err error) *Promise[T] {
	return &Promise[T]{cancel: func() {}, status: closedChannel, err: err}
}

// AwaitFirst will await a group of promises, return either one value or an error,
// and then immediately cancel all other promises in the group.
func AwaitFirst[T any](ps ...*Promise[T]) (T, error) {
	// all unresolved promises are canceled when this function returns
	defer func() {
		for _, p := range ps {
			p.Cancel()
		}
	}()

	// reflection is unfortunately slow, so this happy path might
	// speed things up if a promise is ready. TODO: benchmark this.
	for _, p := range ps {
		select {
		case <-p.status:
			return p.value, p.err
		default:
		}
	}

	// 8 should be more than enough for the common cases, and using a small
	// fixed size for the `make` call will allow it to be stack-allocated
	statuses := make([]reflect.SelectCase, 0, 8)
	for _, p := range ps {
		statuses = append(statuses, reflect.SelectCase{
			Dir:  reflect.SelectRecv,
			Chan: reflect.ValueOf(p.status),
		})
	}

	i, _, _ := reflect.Select(statuses)

	return ps[i].value, ps[i].err
}

// AwaitAll will attempt to await all promises provided. If successful, it will
// return a slice of values, otherwise it will return the first error it encounters
// and cancel any remaining promises.
func AwaitAll[T any](ps ...*Promise[T]) ([]T, error) {
	values := make([]T, 0, len(ps))

	for i, p := range ps {
		v, err := p.Await()
		if err != nil {
			for _, p := range ps[i+1:] {
				p.Cancel()
			}

			return nil, err
		}

		values = append(values, v)
	}

	return values, nil
}
