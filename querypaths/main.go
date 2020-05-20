package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/scionproto/scion/go/lib/addr"
	"github.com/scionproto/scion/go/lib/sciond"
	"github.com/scionproto/scion/go/lib/snet"
	"os"
	"time"
)

var (
	dstIAStr   = flag.String("dstIA", "", "Destination IA address: ISD-AS")
	sciondAddr = flag.String("sciond", sciond.DefaultSCIONDAddress, "SCIOND address")
	refresh    = flag.Bool("refresh", false, "")
)

var (
	dstIA    addr.IA
	execTime time.Duration
)

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
	if *dstIAStr == "" {
		return fmt.Errorf("missing destination IA")
	} else {
		dstIA, err = addr.IAFromString(*dstIAStr)
		if err != nil {
			return fmt.Errorf("unable to parse destination IA")
		}
	}
	ctx := context.Background()
	sdConn, err := sciond.NewService(*sciondAddr).Connect(ctx)
	if err != nil {
		return err
	}
	execTimeRes := make([]int, NbExperiment)
	for i := 0; i < NbExperiment; i++ {
		if _, err := getPaths(sdConn, ctx, dstIA); err != nil {
			return err
		}
		execTimeRes[i] = int(execTime.Milliseconds())
	}
	avgExecTime := 0
	for i := 0; i < NbExperiment; i++ {
		avgExecTime += execTimeRes[i]
	}
	avgExecTime /= NbExperiment
	fmt.Println("Average execution time", avgExecTime, "ms")
	return nil
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
