package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/elazarl/go-bindata-assetfs"
	"github.com/omegaup/quark/broadcaster"
	"github.com/omegaup/quark/common"
	"github.com/omegaup/quark/grader"
	"github.com/omegaup/quark/grader/v1compat"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/net/http2"
	"io"
	"io/ioutil"
	"math/big"
	"net"
	"net/http"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	guidRegex = regexp.MustCompile("^[0-9a-f]{32}$")
)

type graderRunningStatus struct {
	RunnerName string `json:"name"`
	ID         int64  `json:"id"`
}

type graderStatusQueue struct {
	Running           []graderRunningStatus `json:"running"`
	RunQueueLength    int                   `json:"run_queue_length"`
	RunnerQueueLength int                   `json:"runner_queue_length"`
	Runners           []string              `json:"runners"`
}

type graderStatusResponse struct {
	Status            string            `json:"status"`
	BoadcasterSockets int               `json:"broadcaster_sockets"`
	EmbeddedRunner    bool              `json:"embedded_runner"`
	RunningQueue      graderStatusQueue `json:"queue"`
}

type runGradeRequest struct {
	GUIDs   []string `json:"id"`
	Rejudge bool     `json:"rejudge"`
	Debug   bool     `json:"debug"`
}

type runGradeResource struct {
	GUID     string `json:"id"`
	Filename string `json:"filename"`
}

func v1CompatUpdateDatabase(
	ctx *grader.Context,
	db *sql.DB,
	run *grader.RunInfo,
) {
	if run.PenaltyType == "runtime" {
		_, err := db.Exec(
			`UPDATE
				Runs
			SET
				status = 'ready', verdict = ?, runtime = ?, penalty = ?, memory = ?,
				score = ?, contest_score = ?, judged_by = ?
			WHERE
				run_id = ?;`,
			run.Result.Verdict,
			run.Result.Time*1000,
			run.Result.Time*1000,
			run.Result.Memory.Bytes(),
			common.RationalToFloat(run.Result.Score),
			common.RationalToFloat(run.Result.ContestScore),
			run.Result.JudgedBy,
			run.ID,
		)
		if err != nil {
			ctx.Log.Error("Error updating the database", "err", err, "run", run)
		}
	} else {
		_, err := db.Exec(
			`UPDATE
				Runs
			SET
				status = 'ready', verdict = ?, runtime = ?, memory = ?, score = ?,
				contest_score = ?, judged_by = ?
			WHERE
				run_id = ?;`,
			run.Result.Verdict,
			run.Result.Time*1000,
			run.Result.Memory.Bytes(),
			common.RationalToFloat(run.Result.Score),
			common.RationalToFloat(run.Result.ContestScore),
			run.Result.JudgedBy,
			run.ID,
		)
		if err != nil {
			ctx.Log.Error("Error updating the database", "err", err, "run", run)
		}
	}
}

func v1CompatBroadcastRun(
	ctx *grader.Context,
	db *sql.DB,
	client *http.Client,
	run *grader.RunInfo,
) error {
	message := broadcaster.Message{
		Problem: run.ProblemName,
		Public:  false,
	}
	if run.ID == 0 {
		// Ephemeral run. No need to broadcast.
		return nil
	}
	if run.Contest != nil {
		message.Contest = *run.Contest
	}
	type serializedRun struct {
		User         string      `json:"username"`
		Contest      *string     `json:"contest_alias,omitempty"`
		Problemset   *int64      `json:"problemset,omitempty"`
		Problem      string      `json:"alias"`
		GUID         string      `json:"guid"`
		Runtime      float64     `json:"runtime"`
		Penalty      float64     `json:"penalty"`
		Memory       common.Byte `json:"memory"`
		Score        float64     `json:"score"`
		ContestScore float64     `json:"contest_score"`
		Status       string      `json:"status"`
		Verdict      string      `json:"verdict"`
		SubmitDelay  float64     `json:"submit_delay"`
		Time         float64     `json:"time"`
		Language     string      `json:"language"`
	}
	type runFinishedMessage struct {
		Message string        `json:"message"`
		Run     serializedRun `json:"run"`
	}
	msg := runFinishedMessage{
		Message: "/run/update/",
		Run: serializedRun{
			Contest:      run.Contest,
			Problemset:   run.Problemset,
			Problem:      run.ProblemName,
			GUID:         run.GUID,
			Runtime:      run.Result.Time,
			Memory:       run.Result.Memory,
			Score:        common.RationalToFloat(run.Result.Score),
			ContestScore: common.RationalToFloat(run.Result.ContestScore),
			Status:       "ready",
			Verdict:      run.Result.Verdict,
			Language:     run.Run.Language,
			Time:         -1,
			SubmitDelay:  -1,
			Penalty:      -1,
		},
	}

	err := db.QueryRow(
		`SELECT
			u.username, r.penalty, r.submit_delay, UNIX_TIMESTAMP(r.time)
		FROM
			Runs r
		INNER JOIN
			Users u ON u.main_identity_id = r.identity_id
		WHERE
			r.run_id = ?;`, run.ID).Scan(
		&msg.Run.User,
		&msg.Run.Penalty,
		&msg.Run.SubmitDelay,
		&msg.Run.Time,
	)
	if err != nil {
		return err
	}
	message.User = msg.Run.User
	if run.Problemset != nil {
		message.Problemset = *run.Problemset
	}

	marshaled, err := json.Marshal(&msg)
	if err != nil {
		return err
	}

	message.Message = string(marshaled)

	if err := v1CompatBroadcast(ctx, client, &message); err != nil {
		ctx.Log.Error("Error sending run broadcast", "err", err)
	}
	return nil
}

func v1CompatRunPostProcessor(
	db *sql.DB,
	finishedRuns <-chan *grader.RunInfo,
	client *http.Client,
) {
	ctx := context()
	for run := range finishedRuns {
		if run.Result.Verdict == "JE" {
			ctx.Metrics.CounterAdd("grader_runs_je", 1)
		}
		if ctx.Config.Grader.V1.UpdateDatabase {
			v1CompatUpdateDatabase(ctx, db, run)
		}
		if ctx.Config.Grader.V1.SendBroadcast {
			if err := v1CompatBroadcastRun(ctx, db, client, run); err != nil {
				ctx.Log.Error("Error sending run broadcast", "err", err)
			}
		}
	}
}

func v1CompatGetPendingRuns(ctx *grader.Context, db *sql.DB) ([]string, error) {
	rows, err := db.Query(
		`SELECT
			guid
		FROM
			Runs
		WHERE
			status != 'ready';`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	guids := make([]string, 0)
	for rows.Next() {
		var guid string
		err = rows.Scan(&guid)
		if err != nil {
			return nil, err
		}
		guids = append(guids, guid)
	}
	return guids, nil
}

func v1CompatNewRunContext(
	ctx *grader.Context,
	db *sql.DB,
	guid string,
) (*grader.RunContext, *common.ProblemSettings, error) {
	runCtx := grader.NewEmptyRunContext(ctx)
	runCtx.GUID = guid
	runCtx.GradeDir = path.Join(
		ctx.Config.Grader.V1.RuntimeGradePath,
		guid[:2],
		guid[2:],
	)
	var contestName sql.NullString
	var problemset sql.NullInt64
	var penaltyType sql.NullString
	var contestPoints sql.NullFloat64
	err := db.QueryRow(
		`SELECT
			r.run_id, c.alias, r.problemset_id, c.penalty_type, r.language, p.alias,
			pp.points
		FROM
			Runs r
		INNER JOIN
			Problems p ON p.problem_id = r.problem_id
		LEFT JOIN
			Problemset_Problems pp ON pp.problem_id = r.problem_id AND
			pp.problemset_id = r.problemset_id
		LEFT JOIN
			Contests c ON c.problemset_id = pp.problemset_id
		WHERE
			r.guid = ?;`, guid).Scan(
		&runCtx.ID,
		&contestName,
		&problemset,
		&penaltyType,
		&runCtx.Run.Language,
		&runCtx.ProblemName,
		&contestPoints,
	)
	if err != nil {
		return nil, nil, err
	}

	gitProblemInfo, err := v1compat.GetProblemInformation(v1compat.GetRepositoryPath(
		ctx.Config.Grader.V1.RuntimePath,
		runCtx.ProblemName,
	))
	if err != nil {
		return nil, nil, err
	}

	if contestName.Valid {
		runCtx.Contest = &contestName.String
	}
	if problemset.Valid {
		runCtx.Problemset = &problemset.Int64
	}
	if penaltyType.Valid {
		runCtx.PenaltyType = penaltyType.String
	}
	if contestPoints.Valid {
		runCtx.Run.MaxScore = common.FloatToRational(contestPoints.Float64)
	} else {
		runCtx.Run.MaxScore = big.NewRat(1, 1)
	}
	runCtx.Result.MaxScore = runCtx.Run.MaxScore
	contents, err := ioutil.ReadFile(
		path.Join(
			ctx.Config.Grader.V1.RuntimePath,
			"submissions",
			runCtx.GUID[:2],
			runCtx.GUID[2:],
		),
	)
	if err != nil {
		return nil, nil, err
	}
	runCtx.Run.Source = string(contents)

	runCtx.Run.InputHash = gitProblemInfo.InputHash
	return runCtx, gitProblemInfo.Settings, nil
}

func v1CompatInjectRuns(
	ctx *grader.Context,
	runs *grader.Queue,
	db *sql.DB,
	guids []string,
	priority grader.QueuePriority,
) error {
	for _, guid := range guids {
		runCtx, settings, err := v1CompatNewRunContext(ctx, db, guid)
		if err != nil {
			ctx.Log.Error(
				"Error getting run context",
				"err", err,
				"guid", guid,
			)
			return err
		}
		if settings.Slow {
			runCtx.Priority = grader.QueuePriorityLow
		} else {
			runCtx.Priority = priority
		}
		ctx.Log.Info("RunContext", "runCtx", runCtx)
		ctx.Metrics.CounterAdd("grader_runs_total", 1)
		input, err := ctx.InputManager.Add(
			runCtx.Run.InputHash,
			v1compat.NewInputFactory(
				runCtx.ProblemName,
				&ctx.Config,
			),
		)
		if err != nil {
			ctx.Log.Error("Error getting input", "err", err, "run", runCtx)
			return err
		}
		if err = grader.AddRunContext(ctx, runCtx, input); err != nil {
			ctx.Log.Error("Error adding run context", "err", err, "guid", guid)
			return err
		}
		runs.AddRun(runCtx)
	}
	return nil
}

func v1CompatBroadcast(
	ctx *grader.Context,
	client *http.Client,
	message *broadcaster.Message,
) error {
	marshaled, err := json.Marshal(message)
	if err != nil {
		return err
	}

	resp, err := client.Post(
		ctx.Config.Grader.BroadcasterURL,
		"text/json",
		bytes.NewReader(marshaled),
	)
	ctx.Log.Debug("Broadcast", "message", message, "resp", resp, "err", err)
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf(
			"Request to broadcast failed with error code %d",
			resp.StatusCode,
		)
	}
	return nil
}

func registerV1CompatHandlers(mux *http.ServeMux, db *sql.DB) {
	runs, err := context().QueueManager.Get(grader.DefaultQueueName)
	if err != nil {
		panic(err)
	}
	guids, err := v1CompatGetPendingRuns(context(), db)
	if err != nil {
		panic(err)
	}
	for _, guid := range guids {
		if err := v1CompatInjectRuns(
			context(),
			runs,
			db,
			[]string{guid},
			grader.QueuePriorityNormal,
		); err != nil {
			context().Log.Error("Error injecting run", "guid", guid, "err", err)
		}
	}
	context().Log.Info("Injected pending runs", "count", len(guids))

	transport := &http.Transport{
		Dial: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).Dial,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	if !*insecure {
		cert, err := ioutil.ReadFile(context().Config.TLS.CertFile)
		if err != nil {
			panic(err)
		}
		certPool := x509.NewCertPool()
		certPool.AppendCertsFromPEM(cert)
		keyPair, err := tls.LoadX509KeyPair(
			context().Config.TLS.CertFile,
			context().Config.TLS.KeyFile,
		)
		transport.TLSClientConfig = &tls.Config{
			Certificates: []tls.Certificate{keyPair},
			RootCAs:      certPool,
			ClientAuth:   tls.RequireAndVerifyClientCert,
		}
		if err != nil {
			panic(err)
		}
		if err := http2.ConfigureTransport(transport); err != nil {
			panic(err)
		}
	}

	client := &http.Client{Transport: transport}

	finishedRunsChan := make(chan *grader.RunInfo, 1)
	context().InflightMonitor.PostProcessor.AddListener(finishedRunsChan)
	go v1CompatRunPostProcessor(db, finishedRunsChan, client)

	mux.Handle("/", http.FileServer(&wrappedFileSystem{
		fileSystem: &assetfs.AssetFS{
			Asset:     Asset,
			AssetDir:  AssetDir,
			AssetInfo: AssetInfo,
			Prefix:    "data",
		},
	}))

	mux.Handle("/metrics", prometheus.Handler())

	mux.HandleFunc("/grader/status/", func(w http.ResponseWriter, r *http.Request) {
		ctx := context()
		w.Header().Set("Content-Type", "text/json; charset=utf-8")
		runData := ctx.InflightMonitor.GetRunData()
		status := graderStatusResponse{
			Status: "ok",
			RunningQueue: graderStatusQueue{
				Runners: []string{},
				Running: make([]graderRunningStatus, len(runData)),
			},
		}

		for i, data := range runData {
			status.RunningQueue.Running[i].RunnerName = data.Runner
			status.RunningQueue.Running[i].ID = data.ID
		}
		for _, queueInfo := range ctx.QueueManager.GetQueueInfo() {
			for _, l := range queueInfo.Lengths {
				status.RunningQueue.RunQueueLength += l
			}
		}
		encoder := json.NewEncoder(w)
		w.Header().Set("Content-Type", "text/json; charset=utf-8")
		if err := encoder.Encode(&status); err != nil {
			ctx.Log.Error("Error writing /grader/status/ response", "err", err)
		}
	})

	mux.HandleFunc("/run/new/", func(w http.ResponseWriter, r *http.Request) {
		ctx := context()

		if r.Method != "POST" {
			ctx.Log.Error("Invalid request", "url", r.URL.Path, "method", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		tokens := strings.Split(strings.Trim(r.URL.Path, "/"), "/")

		if len(tokens) != 3 {
			ctx.Log.Error("Invalid request", "url", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}

		guid := tokens[2]

		if len(guid) != 32 || !guidRegex.MatchString(guid) {
			ctx.Log.Error("Invalid GUID", "guid", guid)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		filePath := path.Join(
			ctx.Config.Grader.V1.RuntimePath,
			"submissions",
			guid[:2],
			guid[2:],
		)
		f, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0644)
		if err != nil {
			if os.IsExist(err) {
				ctx.Log.Info("/run/new/", "guid", guid, "response", "already exists")
				w.WriteHeader(http.StatusConflict)
				return
			}
			ctx.Log.Info("/run/new/", "guid", guid, "response", "internal server error", "err", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		defer f.Close()

		io.Copy(f, r.Body)

		if err = v1CompatInjectRuns(ctx, runs, db, []string{guid}, grader.QueuePriorityNormal); err != nil {
			ctx.Log.Info("/run/new/", "guid", guid, "response", "internal server error", "err", err)
			w.WriteHeader(http.StatusInternalServerError)
		}
		ctx.Log.Info("/run/new/", "guid", guid, "response", "ok")
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/run/grade/", func(w http.ResponseWriter, r *http.Request) {
		ctx := context()
		decoder := json.NewDecoder(r.Body)
		defer r.Body.Close()

		var request runGradeRequest
		if err := decoder.Decode(&request); err != nil {
			ctx.Log.Error("Error receiving grade request", "err", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		ctx.Log.Info("/run/grade/", "request", request)
		priority := grader.QueuePriorityNormal
		if request.Rejudge || request.Debug {
			priority = grader.QueuePriorityLow
		}
		if err = v1CompatInjectRuns(ctx, runs, db, request.GUIDs, priority); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
		}
		w.Header().Set("Content-Type", "text/json; charset=utf-8")
		fmt.Fprintf(w, "{\"status\":\"ok\"}")
	})

	mux.HandleFunc("/run/payload/", func(w http.ResponseWriter, r *http.Request) {
		ctx := context()
		decoder := json.NewDecoder(r.Body)
		defer r.Body.Close()

		var request runGradeRequest
		if err := decoder.Decode(&request); err != nil {
			ctx.Log.Error("Error receiving grade request", "err", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		ctx.Log.Info("/run/payload/", "request", request)
		var response = make(map[string]*common.Run)
		for _, guid := range request.GUIDs {
			runCtx, _, err := v1CompatNewRunContext(ctx, db, guid)
			if err != nil {
				ctx.Log.Error(
					"Error getting run context",
					"err", err,
					"guid", guid,
				)
				response[guid] = nil
			} else {
				response[guid] = runCtx.Run
			}
		}
		w.Header().Set("Content-Type", "text/json; charset=utf-8")
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		encoder.Encode(response)
	})

	mux.HandleFunc("/run/source/", func(w http.ResponseWriter, r *http.Request) {
		ctx := context()

		if r.Method != "GET" {
			ctx.Log.Error("Invalid request", "url", r.URL.Path, "method", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		tokens := strings.Split(strings.Trim(r.URL.Path, "/"), "/")

		if len(tokens) != 3 {
			ctx.Log.Error("Invalid request", "url", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}

		guid := tokens[2]

		if len(guid) != 32 || !guidRegex.MatchString(guid) {
			ctx.Log.Error("Invalid GUID", "guid", guid)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		filePath := path.Join(
			ctx.Config.Grader.V1.RuntimePath,
			"submissions",
			guid[:2],
			guid[2:],
		)
		f, err := os.Open(filePath)
		if err != nil {
			if os.IsNotExist(err) {
				ctx.Log.Info("/run/source/", "guid", guid, "response", "not found")
				w.WriteHeader(http.StatusNotFound)
				return
			}
			ctx.Log.Info("/run/source/", "guid", guid, "response", "internal server error", "err", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		defer f.Close()

		info, err := f.Stat()
		if err != nil {
			ctx.Log.Info("/run/source/", "guid", guid, "response", "internal server error", "err", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))

		ctx.Log.Info("/run/source/", "guid", guid, "response", "ok")
		w.WriteHeader(http.StatusOK)
		io.Copy(w, f)
	})

	mux.HandleFunc("/run/resource/", func(w http.ResponseWriter, r *http.Request) {
		ctx := context()
		decoder := json.NewDecoder(r.Body)
		defer r.Body.Close()

		var request runGradeResource
		if err := decoder.Decode(&request); err != nil {
			ctx.Log.Error("Error receiving resource request", "err", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		if len(request.GUID) != 32 || !guidRegex.MatchString(request.GUID) {
			ctx.Log.Error("Invalid GUID", "guid", request.GUID)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if request.Filename == "" || strings.HasPrefix(request.Filename, ".") ||
			strings.Contains(request.Filename, "/") {
			ctx.Log.Error("Invalid filename", "filename", request.Filename)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		filePath := path.Join(
			ctx.Config.Grader.V1.RuntimeGradePath,
			request.GUID[:2],
			request.GUID[2:],
			request.Filename,
		)
		f, err := os.Open(filePath)
		if err != nil {
			if os.IsNotExist(err) {
				ctx.Log.Info("/run/resource/", "request", request, "response", "not found")
				w.WriteHeader(http.StatusNotFound)
				return
			}
			ctx.Log.Info("/run/resource/", "request", request, "response", "internal server error", "err", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		defer f.Close()

		info, err := f.Stat()
		if err != nil {
			ctx.Log.Info("/run/resource/", "request", request, "response", "internal server error", "err", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))

		ctx.Log.Info("/run/resource/", "request", request, "response", "ok")
		w.WriteHeader(http.StatusOK)
		io.Copy(w, f)
	})

	mux.HandleFunc("/broadcast/", func(w http.ResponseWriter, r *http.Request) {
		ctx := context()
		decoder := json.NewDecoder(r.Body)
		defer r.Body.Close()

		var message broadcaster.Message
		if err := decoder.Decode(&message); err != nil {
			ctx.Log.Error("Error receiving broadcast request", "err", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		ctx.Log.Info("/broadcast/", "message", message)
		if err := v1CompatBroadcast(ctx, client, &message); err != nil {
			ctx.Log.Error("Error sending broadcast message", "err", err)
		}
		w.Header().Set("Content-Type", "text/json; charset=utf-8")
		fmt.Fprintf(w, "{\"status\":\"ok\"}")
	})

	mux.HandleFunc("/reload-config/", func(w http.ResponseWriter, r *http.Request) {
		ctx := context()
		ctx.Log.Info("/reload-config/")
		w.Header().Set("Content-Type", "text/json; charset=utf-8")
		fmt.Fprintf(w, "{\"status\":\"ok\"}")
	})
}
