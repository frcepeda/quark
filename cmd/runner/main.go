package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"expvar"
	"flag"
	"fmt"
	"github.com/lhchavez/quark/common"
	"github.com/lhchavez/quark/runner"
	"io"
	"io/ioutil"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

var (
	insecure   = flag.Bool("insecure", false, "Do not use TLS")
	configPath = flag.String("config", "/etc/omegaup/runner/config.json",
		"Runner configuration file")
	globalContext atomic.Value
	ioLock        sync.Mutex
	inputManager  *common.InputManager
	minijail      runner.MinijailSandbox
)

func loadContext() error {
	f, err := os.Open(*configPath)
	if err != nil {
		return err
	}
	defer f.Close()

	ctx, err := common.NewContext(f)
	if err != nil {
		return err
	}
	globalContext.Store(ctx)
	return nil
}

func main() {
	rand.Seed(time.Now().UTC().UnixNano())
	flag.Parse()

	if err := loadContext(); err != nil {
		panic(err)
	}

	ctx := globalContext.Load().(*common.Context)
	expvar.Publish("config", &globalContext.Load().(*common.Context).Config)
	inputManager = common.NewInputManager(ctx)
	inputPath := path.Join(ctx.Config.Runner.RuntimePath, "input")
	go inputManager.PreloadInputs(
		inputPath,
		runner.NewRunnerCachedInputFactory(inputPath),
		&ioLock,
	)
	var client *http.Client
	if *insecure {
		client = http.DefaultClient
	} else {
		cert, err := ioutil.ReadFile(ctx.Config.TLS.CertFile)
		if err != nil {
			panic(err)
		}
		certPool := x509.NewCertPool()
		certPool.AppendCertsFromPEM(cert)
		keyPair, err := tls.LoadX509KeyPair(
			ctx.Config.TLS.CertFile,
			ctx.Config.TLS.KeyFile,
		)
		if err != nil {
			panic(err)
		}
		tr := &http.Transport{
			TLSClientConfig: &tls.Config{
				Certificates: []tls.Certificate{keyPair},
				RootCAs:      certPool,
				ClientAuth:   tls.RequireAndVerifyClientCert,
			},
			DisableCompression: true,
		}
		client = &http.Client{Transport: tr}
	}

	baseURL, err := url.Parse(ctx.Config.Runner.GraderURL)
	if err != nil {
		panic(err)
	}

	ctx.Log.Info("omegaUp runner ready to serve")

	var sleepTime float32 = 1

	for {
		if err := processRun(ctx, client, baseURL); err != nil {
			ctx.Log.Error("error grading run", "err", err)
			// Randomized exponential backoff.
			time.Sleep(time.Duration(rand.Float32()*sleepTime) * time.Second)
			if sleepTime < 64 {
				sleepTime *= 2
			}
		} else {
			sleepTime = 1
		}
	}
}

// A reader that blocks until the data is available.
// This is used so that the HTTP connection can be established quickly and then
// block until the results are in. This sends a single byte upon connection
// establishment and relies on the fact that all the data it sends is
// JSON-encoded, so it always sends a '{'.
type blockingReader struct {
	hasRead    bool
	reader     io.Reader
	readerChan chan io.Reader
}

func (r *blockingReader) Read(p []byte) (int, error) {
	if r.reader == nil {
		if !r.hasRead {
			r.hasRead = true
			p[0] = '{'
			return 1, nil
		}
		r.reader = <-r.readerChan
		close(r.readerChan)
		// Make sure the first byte was actually a brace. Otherwise raise an error.
		brace := make([]byte, 1)
		if n, err := r.reader.Read(brace); err != nil || n != 1 || brace[0] != '{' {
			return 0, io.ErrUnexpectedEOF
		}
	}
	return r.reader.Read(p)
}

func processRun(
	ctx *common.Context,
	client *http.Client,
	baseURL *url.URL,
) error {
	requestURL, err := baseURL.Parse("run/request/")
	if err != nil {
		panic(err)
	}
	resp, err := client.Get(requestURL.String())
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	syncID, err := strconv.ParseUint(resp.Header.Get("Sync-ID"), 10, 64)
	if err != nil {
		return err
	}
	ctx.EventCollector.Add(ctx.EventFactory.NewReceiverClockSyncEvent(syncID))

	decoder := json.NewDecoder(resp.Body)
	var run common.Run
	if err := decoder.Decode(&run); err != nil {
		return err
	}
	ctx.EventCollector.Add(ctx.EventFactory.NewEvent(
		"grade",
		common.EventBegin,
		common.Arg{"id", run.AttemptID},
		common.Arg{"input", run.InputHash},
		common.Arg{"language", run.Language},
	))
	defer func() {
		ctx.EventCollector.Add(ctx.EventFactory.NewEvent(
			"grade",
			common.EventEnd,
		))
	}()

	uploadURL, err := baseURL.Parse(fmt.Sprintf("run/%d/results/", run.AttemptID))
	if err != nil {
		return err
	}
	requestBody := &blockingReader{
		readerChan: make(chan io.Reader),
	}
	finished := make(chan error)
	go func() {
		response, err := client.Post(uploadURL.String(), "text/json", requestBody)
		if err != nil {
			finished <- err
		} else {
			response.Body.Close()
			finished <- nil
		}
	}()

	// Make sure no other I/O is being made while we grade this run.
	ioLock.Lock()
	defer ioLock.Unlock()

	inputEvent := ctx.EventFactory.NewCompleteEvent("input")
	input, err := inputManager.Add(
		run.InputHash,
		runner.NewRunnerInputFactory(client, &ctx.Config),
	)
	ctx.EventCollector.Add(inputEvent)
	if err != nil {
		return err
	}
	defer input.Release(input)
	result, err := runner.Grade(ctx, client, baseURL, &run, input, &minijail)
	if err != nil {
		ctx.Log.Error("Error while grading", "err", err)
	}
	var resultBytes bytes.Buffer
	encoder := json.NewEncoder(&resultBytes)
	if err := encoder.Encode(result); err != nil {
		return err
	}
	requestBody.readerChan <- &resultBytes
	return <-finished
}
