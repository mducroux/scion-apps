package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	log "github.com/inconshreveable/log15"
	"github.com/scionproto/scion/go/border/braccept/shared"
	"github.com/scionproto/scion/go/lib/addr"
	"github.com/scionproto/scion/go/lib/common"
	"github.com/scionproto/scion/go/lib/ctrl/seg"
	"github.com/scionproto/scion/go/lib/infra"
	"github.com/scionproto/scion/go/lib/infra/modules/seghandler"
	"github.com/scionproto/scion/go/lib/pathdb/query"
	"github.com/scionproto/scion/go/lib/pathdb/sqlite"
	"github.com/scionproto/scion/go/lib/spath"
	"github.com/scionproto/scion/go/proto"
	"go/build"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
)

// GOPATH is the root of the GOPATH environment.
var GOPATH = build.Default.GOPATH

//// appsRoot is the root location of scionlab apps.
//var appsRoot = flag.String("sabin", path.Join(GOPATH, "bin"),
//	"Path to execute the installed scionlab apps binaries")

// scionRoot is the root location of the scion infrastructure.
var scionRoot = flag.String("sroot", path.Join(GOPATH, "src/github.com/scionproto/scion"),
	"Path to read SCION root directory of infrastructure")
var scionGenCache = flag.String("sgenc", path.Join(*scionRoot, "gen-cache"),
	"Path to read SCION gen-cache directory of infrastructure run-time config")

//var scionGen = flag.String("sgen", path.Join(*scionRoot, "gen"),
//	"Path to read SCION gen directory of infrastructure config")

var filenameDb = flag.String("db", "", "Sqlite DB file")

type Segments struct {
	Segments []Segment `json:"segments"`
}

type Segment struct {
	SrcISD    uint16    `json:"srcISD"`
	SrcAS     string    `json:"srcAS"`
	DstISD    uint16    `json:"dstISD"`
	DstAS     string    `json:"dstAS"`
	NbHops    uint8     `json:"nb_hops"`
	Latency   uint32    `json:"latency"`
	Bandwidth uint64    `json:"bandwidth"`
	ASEntries []ASEntry `json:"ASentries"`
}

type ASEntry struct {
	IA        string  `json:"IA"`
	Latitude  float32 `json:"latitude"`
	Longitude float32 `json:"longitude"`
	Hop       Hop     `json:"hop"`
}

type Hop struct {
	InIA  string `json:"InIA"`
	InIF  uint64 `json:"InIF"`
	OutIA string `json:"OutIA"`
	OutIF uint64 `json:"OutIF"`
}

func main() {
	if err := realMain(); err != nil {
		fmt.Fprintf(os.Stderr, "Error while executing: %v\n", err)
		os.Exit(1)
	}
}

func realMain() error {
	flag.Parse()

	if *filenameDb == "" {
		var err error
		*filenameDb, err = defaultDBFilename()
		if err != nil {
			return err
		}
	}

	log.Info(*filenameDb)

	log.Info("Reading segment file")

	segmentsFile, err := os.Open("segments.txt")
	if err != nil {
		return fmt.Errorf("error opening file")
	}
	defer segmentsFile.Close()

	byteSegments, _ := ioutil.ReadAll(segmentsFile)
	var segments Segments
	if err := json.Unmarshal(byteSegments, &segments); err != nil {
		return fmt.Errorf("error unmarshalling")
	}

	log.Info("Creating segments")

	var toRegister []*seghandler.SegWithHP

	for i := 0; i < len(segments.Segments); i++ {
		scionSegment, err := createScionSegment(segments.Segments[i])
		if err != nil {
			return err
		}
		toRegister = append(toRegister, scionSegment)
	}

	log.Info("Opening path db")

	db, err := sqlite.New(*filenameDb)
	if err != nil {
		return err
	}

	log.Info("Starting transaction")

	tx, err := db.BeginTransaction(context.Background(), nil)
	if err != nil {
		return err
	}
	//defer tx.Rollback()

	for _, segmentToRegister := range toRegister {
		stats, err := tx.InsertWithHPCfgIDs(context.Background(), segmentToRegister.Seg, []*query.HPCfgID{&query.NullHpCfgID})
		if err != nil {
			return err
		}
		if stats.Inserted > 0 {
			log.Info("Inserted: " + segmentToRegister.Seg.Segment.GetLoggingID())
		} else if stats.Updated > 0 {
			log.Info("Updated: " + segmentToRegister.Seg.Segment.GetLoggingID())
		}
		println(segmentToRegister.String())
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	log.Info("Ending transaction")

	_ = db.Close()

	return nil
}

func defaultDBFilename() (string, error) {
	glob := filepath.Join(*scionGenCache, "sd*path.db")
	filenames, err := filepath.Glob(glob)
	if err != nil {
		return "", fmt.Errorf("error while listing files: %v", err)
	}
	if len(filenames) == 1 {
		return filenames[0], nil
	}
	reason := "no"
	if len(filenames) > 1 {
		reason = "more than one"
	}
	return "", fmt.Errorf("found %s files matching '%s', please specify the path to a DB file using the -db flag", reason, glob)
}

func createScionSegment(segment Segment) (*seghandler.SegWithHP, error) {
	infoField := &spath.InfoField{
		ConsDir:  false,
		Shortcut: false,
		Peer:     false,
		TsInt:    shared.TsNow32,
		ISD:      segment.DstISD, //srcISD?
		Hops:     segment.NbHops,
	}
	scionSegment, err := seg.NewSeg(infoField)
	if err != nil {
		return nil, fmt.Errorf("error creating new segment")
	}
	for i := 0; i < len(segment.ASEntries); i++ {
		asEntry := createScionASEntry(segment.ASEntries[i])
		err := scionSegment.AddASEntry(&asEntry, infra.NullSigner)
		if err != nil {
			return nil, fmt.Errorf("error adding as entry")
		}
	}
	segmentWithHP := &seghandler.SegWithHP{
		Seg: &seg.Meta{Type: proto.PathSegTypeFromString("core"), Segment: scionSegment},
	}
	return segmentWithHP, nil
}

func createScionASEntry(asEntry ASEntry) seg.ASEntry {
	rawHop := make([]byte, 8)
	hf := spath.HopField{
		ConsIngress: common.IFIDType(asEntry.Hop.InIF),
		ConsEgress:  common.IFIDType(asEntry.Hop.OutIF),
		ExpTime:     spath.DefaultHopFExpiry,
	}
	hf.Write(rawHop)

	hopEntry := &seg.HopEntry{
		RawInIA:     IAIntFromString(asEntry.Hop.InIA),
		RemoteInIF:  common.IFIDType(asEntry.Hop.InIF),
		InMTU:       1500,
		RawOutIA:    IAIntFromString(asEntry.Hop.OutIA),
		RemoteOutIF: common.IFIDType(asEntry.Hop.OutIF),
		RawHopField: rawHop,
	}

	scionASEntry := seg.ASEntry{
		RawIA:      IAIntFromString(asEntry.IA),
		HopEntries: []*seg.HopEntry{hopEntry}, // can we have more than one?
		MTU:        1500,
		//TrcVer:     scrypto.LatestVer,
		//CertVer:    scrypto.LatestVer,
		//IfIDSize:   0,
	}
	return scionASEntry
}

func IAIntFromString(iaString string) addr.IAInt {
	asEntryIA, err := addr.IAFromString(iaString)
	if err != nil {
		println("Error converting string IA")
		os.Exit(1)
	}
	return asEntryIA.IAInt()
}
