// Package service provides lifecycle helpers for independently runnable
// execution runtime modules.
package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

type Module interface {
	Init(context.Context) error
	Run(context.Context) error
	Close(context.Context) error
}

type NamedModule struct {
	Name   string
	Module Module
}

type Service struct {
	modules []NamedModule
}

func New(modules ...NamedModule) (*Service, error) {
	for _, module := range modules {
		if module.Name == "" {
			return nil, fmt.Errorf("module name is required")
		}
		if module.Module == nil {
			return nil, fmt.Errorf("module %s is nil", module.Name)
		}
	}
	return &Service{modules: append([]NamedModule(nil), modules...)}, nil
}

func (s *Service) Init(ctx context.Context) error {
	for _, module := range s.modules {
		if err := module.Module.Init(ctx); err != nil {
			return fmt.Errorf("init %s: %w", module.Name, err)
		}
	}
	return nil
}

func (s *Service) Run(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	errs := make(chan error, len(s.modules))
	var wg sync.WaitGroup
	for _, module := range s.modules {
		module := module
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := module.Module.Run(runCtx); err != nil && !errors.Is(err, context.Canceled) {
				errs <- fmt.Errorf("run %s: %w", module.Name, err)
			}
		}()
	}

	go func() {
		wg.Wait()
		close(errs)
	}()

	var runErr error
	for err := range errs {
		if err != nil {
			cancel()
			runErr = errors.Join(runErr, err)
		}
	}
	if runErr != nil {
		return runErr
	}
	return ctx.Err()
}

func (s *Service) Close(ctx context.Context) error {
	var closeErr error
	for idx := len(s.modules) - 1; idx >= 0; idx-- {
		module := s.modules[idx]
		if err := module.Module.Close(ctx); err != nil {
			closeErr = errors.Join(closeErr, fmt.Errorf("close %s: %w", module.Name, err))
		}
	}
	return closeErr
}
