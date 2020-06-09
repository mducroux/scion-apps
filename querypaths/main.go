package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/scionproto/scion/go/lib/addr"
	"github.com/scionproto/scion/go/lib/sciond"
	"github.com/scionproto/scion/go/lib/snet"
	"io/ioutil"
	"os"
	"time"
)

var (
	sciondAddr = flag.String("sciond", sciond.DefaultSCIONDAddress, "SCIOND address")
	refresh    = flag.Bool("refresh", false, "")
)

var (
	execTime time.Duration
)

type PathSamples struct {
	Samples []Sample `json:"path_samples"`
}

type Sample struct {
	IA      string `json:"IA"`
	NbPaths int    `json:"nb_paths"`
}

type OutputExperiment struct {
	Results []ResultExperiment
}

type ResultExperiment struct {
	NbPaths  int
	ExecTime int
}

const NbExperiment = 100

func main() {
	if err := realMain(); err != nil {
		fmt.Fprintf(os.Stderr, "Error while executing: %v\n", err)
		os.Exit(1)
	}
}

func realMain() error {
	flag.Parse()
	var err error

	ctx := context.Background()
	sdConn, err := sciond.NewService(*sciondAddr).Connect(ctx)
	if err != nil {
		return err
	}

	data, err := ioutil.ReadFile("path_samples.json")
	if err != nil {
		return fmt.Errorf("error reading file")
	}

	var pathSamples PathSamples
	if err := json.Unmarshal(data, &pathSamples); err != nil {
		return fmt.Errorf("error unmarshalling")
	}

	results := make([]ResultExperiment, len(pathSamples.Samples))
	for i := 0; i < len(pathSamples.Samples); i++ {
		result, err := runExperiment(pathSamples.Samples[i], ctx, sdConn)
		if err != nil {
			return err
		}
		results[i] = result
	}

	outputData := OutputExperiment{
		Results: results,
	}
	file, _ := json.MarshalIndent(outputData, "", " ")
	_ = ioutil.WriteFile("output_experiment.json", file, 0644)

	return nil
}

func runExperiment(sample Sample, ctx context.Context, sdConn sciond.Connector) (ResultExperiment, error) {
	execTimeRes := make([]int, NbExperiment)
	dstIA, err := addr.IAFromString(sample.IA)
	if err != nil {
		return ResultExperiment{}, err
	}

	for i := 0; i < NbExperiment; i++ {
		if _, err := getPaths(sdConn, ctx, dstIA); err != nil {
			return ResultExperiment{}, err
		}
		execTimeRes[i] = int(execTime.Milliseconds())
	}
	avgExecTime := 0
	for i := 0; i < NbExperiment; i++ {
		avgExecTime += execTimeRes[i]
	}
	avgExecTime /= NbExperiment

	result := ResultExperiment{
		NbPaths:  sample.NbPaths,
		ExecTime: avgExecTime,
	}

	return result, nil
}

func measureTime() func() {
	start := time.Now()
	return func() {
		execTime = time.Since(start)
	}
}

func getPaths(sdConn sciond.Connector, ctx context.Context, dstIA addr.IA) ([]snet.Path, error) {
	defer measureTime()()
	_, err := sdConn.Paths(ctx, dstIA, addr.IA{},
		sciond.PathReqFlags{Refresh: *refresh})
	if err != nil {
		return nil, err
	}
	return nil, nil
}
