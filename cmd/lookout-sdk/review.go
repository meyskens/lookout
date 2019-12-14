package main

import (
	"context"
	"os"
	"time"

	"github.com/meyskens/lookout"
	"github.com/meyskens/lookout/provider/json"
	"github.com/meyskens/lookout/server"

	"gopkg.in/meyskens/lookout-sdk.v0/pb"

	uuid "github.com/satori/go.uuid"
	gocli "gopkg.in/src-d/go-cli.v0"
)

func init() {
	app.AddCommand(&ReviewCommand{})
}

type ReviewCommand struct {
	gocli.PlainCommand `name:"review" short-description:"trigger a review event" long-description:"Provides a simple data server and triggers an analyzer review event"`
	EventCommand
}

func (c *ReviewCommand) Execute(args []string) error {
	stopCh := make(chan error, 1)

	if err := c.openRepository(); err != nil {
		return err
	}

	fromRef, toRef, err := c.resolveRefs()
	if err != nil {
		return err
	}

	conf, err := c.parseConfig()
	if err != nil {
		return err
	}

	dataHandler, err := c.makeDataServerHandler()
	if err != nil {
		return err
	}

	startDataServer, stopDataServer := c.initDataServer(dataHandler)
	go func() {
		stopCh <- startDataServer()
	}()

	analyzer, err := c.analyzer()
	if err != nil {
		return err
	}

	srv := server.NewServer(server.Options{
		Poster:     json.NewPoster(os.Stdout),
		FileGetter: dataHandler.FileGetter,
		Analyzers: map[string]lookout.Analyzer{
			analyzer.Config.Name: analyzer,
		},
		ExitOnError: true,
	})

	id, err := uuid.NewV4()
	if err != nil {
		return err
	}

	err = srv.HandleReview(context.TODO(), &lookout.ReviewEvent{
		ReviewEvent: pb.ReviewEvent{
			InternalID:  id.String(),
			CreatedAt:   time.Now(),
			UpdatedAt:   time.Now(),
			IsMergeable: true,
			Source:      *toRef,
			CommitRevision: lookout.CommitRevision{
				Base: *fromRef,
				Head: *toRef,
			},
			Configuration: conf}},
		false)

	stopDataServer()

	if err != nil {
		return err
	}

	return <-stopCh
}
