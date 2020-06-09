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
	"github.com/scionproto/scion/go/lib/infra/modules/trust"
	"github.com/scionproto/scion/go/lib/infra/modules/trust/trustdbsqlite"
	"github.com/scionproto/scion/go/lib/keyconf"
	"github.com/scionproto/scion/go/lib/pathdb/query"
	"github.com/scionproto/scion/go/lib/pathdb/sqlite"
	"github.com/scionproto/scion/go/lib/spath"
	"github.com/scionproto/scion/go/proto"
	"go/build"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"sort"
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
var scionGen = flag.String("sgen", path.Join(*scionRoot, "gen"),
	"Path to read SCION gen directory of infrastructure config")

var filenamePathDb = flag.String("pathdb", "", "Sqlite DB file")

var filenameTrustDb = flag.String("trustdb", "", "Sqlite DB file")

type Segments struct {
	Segments []Segment `json:"segments"`
}

type Segment struct {
	SrcISD uint16 `json:"srcISD"`
	SrcAS  string `json:"srcAS"`
	DstISD uint16 `json:"dstISD"`
	DstAS  string `json:"dstAS"`
	NbHops uint8  `json:"nb_hops"`
	//Latency   uint32    `json:"latency"`
	//Bandwidth uint64    `json:"bandwidth"`
	ASEntries []ASEntry `json:"ASentries"`
}

type ASEntry struct {
	IA string `json:"IA"`
	//Latitude  float32 `json:"latitude"`
	//Longitude float32 `json:"longitude"`
	Hop Hop `json:"hop"`
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

	if *filenamePathDb == "" {
		var err error
		*filenamePathDb, err = defaultPathDBFilename()
		if err != nil {
			return err
		}
	}

	if *filenameTrustDb == "" {
		var err error
		*filenameTrustDb, err = defaultTrustDBFilename()
		if err != nil {
			return err
		}
	}

	log.Info("Reading segment file")
	data, err := ioutil.ReadFile("path_db_segments.json")
	if err != nil {
		return fmt.Errorf("error reading file")
	}
	var segments Segments
	if err := json.Unmarshal(data, &segments); err != nil {
		return fmt.Errorf("error unmarshalling")
	}

	log.Info("Opening trust_db: " + (*filenameTrustDb))
	trustDb, err := trustdbsqlite.New(*filenameTrustDb)
	if err != nil {
		return err
	}
	defer func() {
		_ = trustDb.Close()
	}()

	provider := trust.Provider{
		DB: trustDb,
	}
	inspector := trust.DefaultInspector{Provider: provider} // maybe useless
	inserter := trust.DefaultInserter{BaseInserter: trust.BaseInserter{DB: trustDb}}
	trustStore := trust.Store{
		Inserter:       inserter,
		DB:             trustDb,
		CryptoProvider: provider,
		Inspector:      inspector,
	}

	log.Info("Inserting chain certificates")
	if err := insertChain(context.Background(), trustStore); err != nil {
		return err
	}

	log.Info("Inserting TRCs")
	if err := insertTRC(context.Background(), trustStore); err != nil {
		return err
	}

	log.Info("Creating signers")
	signers, err := createSigners(context.Background(), trustStore)
	if err != nil {
		return err
	}

	log.Info("Creating segments")
	var toRegister []*seghandler.SegWithHP
	for i := 0; i < len(segments.Segments); i++ {
		scionSegment, err := createScionSegment(segments.Segments[i], signers)
		if err != nil {
			return err
		}
		toRegister = append(toRegister, scionSegment)
	}
	// Sort to prevent sql deadlock.
	sort.Slice(toRegister, func(i, j int) bool {
		return toRegister[i].Seg.Segment.GetLoggingID() < toRegister[j].Seg.Segment.GetLoggingID()
	})

	log.Info("Opening path_db: " + (*filenamePathDb))
	pathDb, err := sqlite.New(*filenamePathDb)
	if err != nil {
		return err
	}
	defer func() {
		_ = pathDb.Close()
	}()

	log.Info("Starting pathdb transaction")
	tx, err := pathDb.BeginTransaction(context.Background(), nil)
	if err != nil {
		return err
	}
	defer func() {
		_ = tx.Rollback() // The rollback will be ignored if the tx has been committed later in the function.
	}()
	for _, segmentToRegister := range toRegister {
		_, err := tx.InsertWithHPCfgIDs(context.Background(), segmentToRegister.Seg, []*query.HPCfgID{&query.NullHpCfgID})
		if err != nil {
			return err
		}
		//if stats.Inserted > 0 {
		//	log.Info("Inserted: " + segmentToRegister.Seg.Segment.GetLoggingID())
		//} else if stats.Updated > 0 {
		//	log.Info("Updated: " + segmentToRegister.Seg.Segment.GetLoggingID())
		//}
		//println(segmentToRegister.String())
	}

	log.Info("Committing pathdb transaction")
	if err := tx.Commit(); err != nil {
		return err
	}

	log.Info("Ending pathdb transaction")
	return nil
}

func createSigners(ctx context.Context, trustStore trust.Store) (*map[addr.IA]infra.Signer, error) {
	signers := make(map[addr.IA]infra.Signer)
	isdDirs, err := filepath.Glob(fmt.Sprintf("%s/ISD*", *scionGen))
	if err != nil {
		return nil, err
	}
	for _, isdDir := range isdDirs {
		asDirs, err := filepath.Glob(fmt.Sprintf("%s/AS*", isdDir))
		if err != nil {
			return nil, err
		}
		for _, asDir := range asDirs {
			isd, err := addr.ISDFromString(filepath.Base(isdDir)[3:])
			if err != nil {
				return nil, err
			}
			as, err := addr.ASFromFileFmt(filepath.Base(asDir), true)
			if err != nil {
				return nil, err
			}
			ia := addr.IA{I: isd, A: as}
			signer, err := createSigner(ctx, ia, trustStore)
			if err != nil {
				return nil, err
			}
			signers[ia] = signer
		}
	}
	return &signers, nil
}

func createSigner(ctx context.Context, ia addr.IA, trustStore trust.Store) (infra.Signer, error) {
	//log.Info(filepath.Join(fmt.Sprintf("%s/ISD%d/AS%s", *scionGen, ia.I, ia.A.FileFmt()), "keys"))
	gen := trust.SignerGen{
		IA: ia,
		KeyRing: keyconf.LoadingRing{
			Dir: filepath.Join(fmt.Sprintf("%s/ISD%d/AS%s", *scionGen, ia.I, ia.A.FileFmt()), "keys"),
			IA:  ia,
		},
		Provider: trustStore,
	}
	signer, err := gen.Signer(ctx)
	if err != nil {
		return infra.NullSigner, err
	}
	return signer, nil
}

func insertTRC(ctx context.Context, trustStore trust.Store) error {
	isdDirs, err := filepath.Glob(fmt.Sprintf("%s/ISD*", *scionGen))
	if err != nil {
		return err
	}
	for _, isdDir := range isdDirs {
		trcDir := filepath.Join(isdDir, "trcs")
		if err := trustStore.LoadTRCs(ctx, trcDir); err != nil {
			return err
		}
	}
	return nil
}

func insertChain(ctx context.Context, trustStore trust.Store) error {
	isdDirs, err := filepath.Glob(fmt.Sprintf("%s/ISD*", *scionGen))
	if err != nil {
		return err
	}
	for _, isdDir := range isdDirs {
		asDirs, err := filepath.Glob(fmt.Sprintf("%s/AS*", isdDir))
		if err != nil {
			return err
		}
		for _, asDir := range asDirs {
			if err := trustStore.LoadChains(ctx, path.Join(asDir, "certs")); err != nil {
				return err
			}
		}
	}
	return nil
}

func defaultPathDBFilename() (string, error) {
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
	return "", fmt.Errorf("found %s files matching '%s', please specify the path to a pathDB file using the -pathdb flag", reason, glob)
}

func defaultTrustDBFilename() (string, error) {
	glob := filepath.Join(*scionGenCache, "sd*trust.db")
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
	return "", fmt.Errorf("found %s files matching '%s', please specify the path to a trustDB file using the -trustdb flag", reason, glob)
}

func createScionSegment(segment Segment, signers *map[addr.IA]infra.Signer) (*seghandler.SegWithHP, error) {
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
		ia, err := addr.IAFromString(segment.ASEntries[i].IA)
		if err != nil {
			return nil, err
		}
		if err := scionSegment.AddASEntry(&asEntry, (*signers)[ia]); err != nil {
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
		ExpTime:     spath.MaxTTLField,
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
		TrcVer:     1,
		CertVer:    1,
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
