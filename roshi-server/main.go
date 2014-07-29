// roshi-server provides a REST-y HTTP service to interact with a farm.
package main

import (
	"bytes"
	"encoding/json"
	_ "expvar"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/soundcloud/roshi/cluster"
	"github.com/soundcloud/roshi/common"
	"github.com/soundcloud/roshi/farm"
	"github.com/soundcloud/roshi/instrumentation"
	"github.com/soundcloud/roshi/instrumentation/prometheus"
	"github.com/soundcloud/roshi/instrumentation/statsd"
	"github.com/soundcloud/roshi/pool"

	"github.com/gorilla/pat"
	"github.com/peterbourgon/g2s"
)

func main() {
	var (
		redisInstances             = flag.String("redis.instances", "", "Semicolon-separated list of comma-separated lists of Redis instances")
		redisConnectTimeout        = flag.Duration("redis.connect.timeout", 3*time.Second, "Redis connect timeout")
		redisReadTimeout           = flag.Duration("redis.read.timeout", 3*time.Second, "Redis read timeout")
		redisWriteTimeout          = flag.Duration("redis.write.timeout", 3*time.Second, "Redis write timeout")
		redisMCPI                  = flag.Int("redis.mcpi", 10, "Max connections per Redis instance")
		redisHash                  = flag.String("redis.hash", "murmur3", "Redis hash function: murmur3, fnv, fnva")
		farmWriteQuorum            = flag.String("farm.write.quorum", "51%", "Write quorum, either number of clusters (2) or percentage of clusters (51%)")
		farmReadStrategy           = flag.String("farm.read.strategy", "SendAllReadAll", "Farm read strategy: SendAllReadAll, SendOneReadOne, SendAllReadFirstLinger, SendVarReadFirstLinger")
		farmReadThresholdRate      = flag.Int("farm.read.threshold.rate", 2000, "Baseline SendAll keys read per sec, additional keys are SendOne (SendVarReadFirstLinger strategy only)")
		farmReadThresholdLatency   = flag.Duration("farm.read.threshold.latency", 50*time.Millisecond, "If a SendOne read has not returned anything after this latency, it's promoted to SendAll (SendVarReadFirstLinger strategy only)")
		farmRepairStrategy         = flag.String("farm.repair.strategy", "RateLimitedRepairs", "Farm repair strategy: AllRepairs, NoRepairs, RateLimitedRepairs")
		farmRepairMaxKeysPerSecond = flag.Int("farm.repair.max.keys.per.second", 1000, "Max repaired keys per second (RateLimited repairer only)")
		maxSize                    = flag.Int("max.size", 10000, "Maximum number of events per key")
		selectGap                  = flag.Duration("select.gap", 0*time.Millisecond, "delay between pipeline read invocations when Selecting over multiple keys")
		statsdAddress              = flag.String("statsd.address", "", "Statsd address (blank to disable)")
		statsdSampleRate           = flag.Float64("statsd.sample.rate", 0.1, "Statsd sample rate for normal metrics")
		statsdBucketPrefix         = flag.String("statsd.bucket.prefix", "myservice.", "Statsd bucket key prefix, including trailing period")
		prometheusNamespace        = flag.String("prometheus.namespace", "roshiserver", "Prometheus key namespace, excluding trailing punctuation")
		prometheusMaxSummaryAge    = flag.Duration("prometheus.max.summary.age", 10*time.Second, "Prometheus max age for instantaneous histogram data")
		httpAddress                = flag.String("http.address", ":6302", "HTTP listen address")
	)
	flag.Parse()
	log.SetOutput(os.Stdout)
	log.SetFlags(log.Lmicroseconds)
	log.Printf("GOMAXPROCS %d", runtime.GOMAXPROCS(-1))

	// Set up statsd instrumentation, if it's specified.
	statter := g2s.Noop()
	if *statsdAddress != "" {
		var err error
		statter, err = g2s.Dial("udp", *statsdAddress)
		if err != nil {
			log.Fatal(err)
		}
	}
	prometheusInstr := prometheus.New(*prometheusNamespace, *prometheusMaxSummaryAge)
	prometheusInstr.Install("/metrics", http.DefaultServeMux)
	instr := instrumentation.NewMultiInstrumentation(
		statsd.New(statter, float32(*statsdSampleRate), *statsdBucketPrefix),
		prometheusInstr,
	)

	// Parse read strategy.
	var readStrategy farm.ReadStrategy
	switch strings.ToLower(*farmReadStrategy) {
	case "sendallreadall":
		readStrategy = farm.SendAllReadAll
	case "sendonereadone":
		readStrategy = farm.SendOneReadOne
	case "sendallreadfirstlinger":
		readStrategy = farm.SendAllReadFirstLinger
	case "sendvarreadfirstlinger":
		readStrategy = farm.SendVarReadFirstLinger(*farmReadThresholdRate, *farmReadThresholdLatency)
	default:
		log.Fatalf("unknown read strategy %q", *farmReadStrategy)
	}
	log.Printf("using %s read strategy", *farmReadStrategy)

	// Parse repair strategy. Note that because this is a client-facing
	// production server, all repair strategies get a Nonblocking wrapper!
	repairRequestBufferSize := 100
	var repairStrategy farm.RepairStrategy
	switch strings.ToLower(*farmRepairStrategy) {
	case "allrepairs":
		repairStrategy = farm.Nonblocking(repairRequestBufferSize, farm.AllRepairs)
	case "norepairs":
		repairStrategy = farm.Nonblocking(repairRequestBufferSize, farm.NoRepairs)
	case "ratelimitedrepairs":
		repairStrategy = farm.Nonblocking(repairRequestBufferSize, farm.RateLimited(*farmRepairMaxKeysPerSecond, farm.AllRepairs))
	default:
		log.Fatalf("unknown repair strategy %q", *farmRepairStrategy)
	}
	log.Printf("using %s repair strategy", *farmRepairStrategy)

	// Parse hash function.
	var hashFunc func(string) uint32
	switch strings.ToLower(*redisHash) {
	case "murmur3":
		hashFunc = pool.Murmur3
	case "fnv":
		hashFunc = pool.FNV
	case "fnva":
		hashFunc = pool.FNVa
	default:
		log.Fatalf("unknown hash %q", *redisHash)
	}

	// Build the farm.
	farm, err := newFarm(
		*redisInstances,
		*farmWriteQuorum,
		*redisConnectTimeout, *redisReadTimeout, *redisWriteTimeout,
		*redisMCPI,
		hashFunc,
		readStrategy,
		repairStrategy,
		*maxSize,
		*selectGap,
		instr,
	)
	if err != nil {
		log.Fatal(err)
	}

	// Build the HTTP server.
	r := pat.New()
	r.Add("GET", "/metrics", http.DefaultServeMux)
	r.Add("GET", "/debug", http.DefaultServeMux)
	r.Get("/", handleSelect(farm))
	r.Post("/", handleInsert(farm))
	r.Delete("/", handleDelete(farm))
	h := http.Handler(r)

	// Go for it.
	log.Printf("listening on %s", *httpAddress)
	log.Fatal(http.ListenAndServe(*httpAddress, h))
}

func newFarm(
	redisInstances string,
	writeQuorumStr string,
	connectTimeout, readTimeout, writeTimeout time.Duration,
	redisMCPI int,
	hash func(string) uint32,
	readStrategy farm.ReadStrategy,
	repairStrategy farm.RepairStrategy,
	maxSize int,
	selectGap time.Duration,
	instr instrumentation.Instrumentation,
) (*farm.Farm, error) {
	clusters, err := farm.ParseFarmString(
		redisInstances,
		connectTimeout,
		readTimeout,
		writeTimeout,
		redisMCPI,
		hash,
		maxSize,
		selectGap,
		instr,
	)
	if err != nil {
		return nil, err
	}
	log.Printf("%d cluster(s)", len(clusters))

	writeQuorum, err := evaluateScalarPercentage(
		writeQuorumStr,
		len(clusters),
	)
	if err != nil {
		return nil, err
	}

	return farm.New(
		clusters,
		writeQuorum,
		readStrategy,
		repairStrategy,
		instr,
	), nil
}

func handleSelect(selecter farm.Selecter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		began := time.Now()

		if err := r.ParseForm(); err != nil {
			respondError(w, r.Method, r.URL.String(), http.StatusInternalServerError, err)
			return
		}
		offset := parseInt(r.Form, "offset", 0)
		limit := parseInt(r.Form, "limit", 10)
		coalesce := parseBool(r.Form, "coalesce", false)

		var keys [][]byte
		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(&keys); err != nil {
			respondError(w, r.Method, r.URL.String(), http.StatusBadRequest, err)
			return
		}

		keyStrings := make([]string, len(keys))
		for i := range keys {
			keyStrings[i] = string(keys[i])
		}

		var records interface{}
		if coalesce {
			// We need to Select from 0 to offset+limit, flatten the map to a
			// single ordered slice, and then cut off the last limit elements.
			m, err := selecter.Select(keyStrings, 0, offset+limit)
			if err != nil {
				respondError(w, r.Method, r.URL.String(), http.StatusInternalServerError, err)
				return
			}
			records = flatten(m, offset, limit)
		} else {
			// We can directly Select using the given offset and limit.
			m, err := selecter.Select(keyStrings, offset, limit)
			if err != nil {
				respondError(w, r.Method, r.URL.String(), http.StatusInternalServerError, err)
				return
			}
			records = m
		}

		respondSelected(w, keys, offset, limit, records, time.Since(began))
	}
}

func handleInsert(inserter cluster.Inserter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		began := time.Now()

		var tuples []common.KeyScoreMember
		if err := json.NewDecoder(r.Body).Decode(&tuples); err != nil {
			respondError(w, r.Method, r.URL.String(), http.StatusBadRequest, err)
			return
		}

		if err := inserter.Insert(tuples); err != nil {
			respondError(w, r.Method, r.URL.String(), http.StatusInternalServerError, err)
			return
		}

		respondInserted(w, len(tuples), time.Since(began))
	}
}

func handleDelete(deleter cluster.Deleter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		began := time.Now()

		var tuples []common.KeyScoreMember
		if err := json.NewDecoder(r.Body).Decode(&tuples); err != nil {
			respondError(w, r.Method, r.URL.String(), http.StatusBadRequest, err)
			return
		}

		if err := deleter.Delete(tuples); err != nil {
			respondError(w, r.Method, r.URL.String(), http.StatusInternalServerError, err)
			return
		}

		respondDeleted(w, len(tuples), time.Since(began))
	}
}

func flatten(m map[string][]common.KeyScoreMember, offset, limit int) []common.KeyScoreMember {
	a := []common.KeyScoreMember{}
	for _, tuples := range m {
		a = append(a, tuples...)
	}
	sort.Sort(keyScoreMembers(a))
	if len(a) < offset {
		return []common.KeyScoreMember{}
	}
	a = a[offset:]
	if len(a) > limit {
		a = a[:limit]
	}
	return a
}

func parseInt(values url.Values, key string, defaultValue int) int {
	value, err := strconv.ParseInt(values.Get(key), 10, 64)
	if err != nil {
		return defaultValue
	}
	return int(value)
}

func parseBool(values url.Values, key string, defaultValue bool) bool {
	value, err := strconv.ParseBool(values.Get(key))
	if err != nil {
		return defaultValue
	}
	return value
}

func respondInserted(w http.ResponseWriter, n int, duration time.Duration) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"inserted": n,
		"duration": duration.String(),
	})
}

func respondSelected(w http.ResponseWriter, keys [][]byte, offset, limit int, records interface{}, duration time.Duration) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"keys":     keys,
		"offset":   offset,
		"limit":    limit,
		"records":  records,
		"duration": duration.String(),
	})
}

func respondDeleted(w http.ResponseWriter, n int, duration time.Duration) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"deleted":  n,
		"duration": duration.String(),
	})
}

func respondError(w http.ResponseWriter, method, url string, code int, err error) {
	log.Printf("%s %s: HTTP %d: %s", method, url, code, err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error":       err.Error(),
		"code":        code,
		"description": http.StatusText(code),
	})
}

// evaluateScalarPercentage takes a string of the form "P%" (percent) or "S"
// (straight scalar value), and evaluates that against the passed total n.
// Percentages mean at least that percent; for example, "50%" of 3 evaluates
// to 2. It is an error if the passed string evaluates to less than 1 or more
// than n.
func evaluateScalarPercentage(s string, n int) (int, error) {
	if n <= 0 {
		return -1, fmt.Errorf("n must be at least 1")
	}

	s = strings.TrimSpace(s)
	var value int
	if strings.HasSuffix(s, "%") {
		percentInt, err := strconv.ParseInt(s[:len(s)-1], 10, 64)
		if err != nil || percentInt <= 0 || percentInt > 100 {
			return -1, fmt.Errorf("bad percentage input %q", s)
		}
		value = int(math.Ceil((float64(percentInt) / 100.0) * float64(n)))
	} else {
		value64, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return -1, fmt.Errorf("bad scalar input %q", s)
		}
		value = int(value64)
	}
	if value <= 0 || value > n {
		return -1, fmt.Errorf("with n=%d, value=%d (from %q) is invalid", n, value, s)
	}
	return value, nil
}

type keyScoreMembers []common.KeyScoreMember

func (a keyScoreMembers) Len() int      { return len(a) }
func (a keyScoreMembers) Swap(i, j int) { a[i], a[j] = a[j], a[i] }

// These two sort types copy the sorting that Redis does internally.
//
// ZREVRANGEBYSCORE (normal, limit >= 0, new-to-old): highest score first,
// then reverse lexicographical of members.
//
// ZRANGEBYSCORE (special, limit < 0, old-to-new): lowest score first, then
// forward lexicographical of members.

type newToOld struct{ keyScoreMembers }

func (a newToOld) Less(i, j int) bool {
	if a.keyScoreMembers[i].Score != a.keyScoreMembers[j].Score {
		return a.keyScoreMembers[i].Score > a.keyScoreMembers[j].Score // higher score = newer
	}
	// If same score, sort from from z -> a
	return bytes.Compare([]byte(a.keyScoreMembers[i].Member), []byte(a.keyScoreMembers[j].Member)) > 0
}

type oldToNew struct{ keyScoreMembers }

func (a oldToNew) Less(i, j int) bool {
	if a.keyScoreMembers[i].Score != a.keyScoreMembers[j].Score {
		return a.keyScoreMembers[i].Score < a.keyScoreMembers[j].Score // lower score = older
	}
	// If same score, sort from from a -> z
	return bytes.Compare([]byte(a.keyScoreMembers[i].Member), []byte(a.keyScoreMembers[j].Member)) < 0
}
