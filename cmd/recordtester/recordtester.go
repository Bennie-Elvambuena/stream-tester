// Record tester is a tool to test Livepeer API's recording functionality
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"math/rand"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/golang/glog"
	serfClient "github.com/hashicorp/serf/client"
	"github.com/livepeer/go-api-client"
	"github.com/livepeer/joy4/format"
	"github.com/livepeer/livepeer-data/pkg/client"
	"github.com/livepeer/stream-tester/internal/app/common"
	"github.com/livepeer/stream-tester/internal/app/recordtester"
	"github.com/livepeer/stream-tester/internal/app/transcodetester"
	"github.com/livepeer/stream-tester/internal/app/vodtester"
	"github.com/livepeer/stream-tester/internal/metrics"
	"github.com/livepeer/stream-tester/internal/server"
	"github.com/livepeer/stream-tester/internal/testers"
	"github.com/livepeer/stream-tester/internal/utils"
	"github.com/livepeer/stream-tester/messenger"
	"github.com/livepeer/stream-tester/model"
	"github.com/peterbourgon/ff/v2"
	"golang.org/x/sync/errgroup"
)

type geolocateSerfNodes struct {
	Key       string
	Latitude  float64
	Longitude float64
	Distance  float64
	Nodes     []serfClient.Member
}

func init() {
	format.RegisterAll()
	rand.Seed(time.Now().UnixNano())
}

func findInSlice(slice []geolocateSerfNodes, key string) int {
	for i, v := range slice {
		if v.Key == key {
			return i
		}
	}
	return -1
}

const (
	earthRadius = 6371 // Earth's radius in kilometers
)

func getDistance(lat1, lon1, lat2, lon2 float64) float64 {
	// convert to radians
	lat1 = lat1 * math.Pi / 180
	lon1 = lon1 * math.Pi / 180
	lat2 = lat2 * math.Pi / 180
	lon2 = lon2 * math.Pi / 180

	// haversine formula
	a := math.Sin(lat2-lat1)*math.Sin(lat2-lat1) +
		math.Cos(lat1)*math.Cos(lat2)*
			math.Sin(lon2-lon1)*math.Sin(lon2-lon1)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))

	distance := earthRadius * c

	return distance
}

func getSerfMembers(useSerf bool, serfRPCAddr string) ([]serfClient.Member, error) {
	if !useSerf || serfRPCAddr == "" {
		return nil, nil
	}
	rpcClient, err := serfClient.NewRPCClient(serfRPCAddr)
	if err != nil {
		glog.Error(err)
		return nil, err
	}
	members, err := rpcClient.Members()
	if err != nil {
		glog.Error(err)
		return nil, err
	}
	return members, nil
}

func getMemberLocation(member serfClient.Member) (string, string) {
	return member.Tags["latitude"], member.Tags["longitude"]
}

func getClosestSerfNodes(serfMembers []serfClient.Member, selectNodeCount int, lat, long float64) []serfClient.Member {
	locationKey := func(x, y string) string {
		return fmt.Sprintf("%s-%s", x, y)
	}
	if serfMembers == nil {
		return nil
	}
	var nodeDistances []geolocateSerfNodes
	for _, member := range serfMembers {
		x2, y2 := getMemberLocation(member)
		key := locationKey(x2, y2)
		index := findInSlice(nodeDistances, key)
		if index == -1 {
			lat2, _ := strconv.ParseFloat(x2, 64)
			long2, _ := strconv.ParseFloat(y2, 64)
			distance := getDistance(lat, long, lat2, long2)
			node := geolocateSerfNodes{
				Key:       key,
				Latitude:  lat2,
				Longitude: long2,
				Distance:  distance,
				Nodes:     []serfClient.Member{member},
			}
			nodeDistances = append(nodeDistances, node)
		} else {
			node := nodeDistances[index]
			node.Nodes = append(node.Nodes, member)
			nodeDistances[index] = node
		}
	}
	sort.Slice(nodeDistances, func(i, j int) bool {
		return nodeDistances[i].Distance <= nodeDistances[j].Distance
	})
	var returnMembers []serfClient.Member
	index := 0
	for (len(returnMembers) < selectNodeCount) && (index < len(nodeDistances)) {
		returnMembers = append(returnMembers, nodeDistances[index].Nodes...)
		index++
	}
	return returnMembers
}

func main() {
	flag.Set("logtostderr", "true")
	vFlag := flag.Lookup("v")

	fs := flag.NewFlagSet("recordtester", flag.ExitOnError)

	verbosity := fs.String("v", "", "Log verbosity.  {4|5|6}")
	version := fs.Bool("version", false, "Print out the version")

	sim := fs.Int("sim", 0, "Load test using <sim> streams")
	testDuration := fs.Duration("test-dur", 0, "How long to run overall test")
	pauseDuration := fs.Duration("pause-dur", 0, "How long to wait between two consecutive RTMP streams that will comprise one user session")
	taskPollDuration := fs.Duration("task-poll-dur", 15*time.Second, "How long to wait between polling for task status")
	apiToken := fs.String("api-token", "", "Token of the Livepeer API to be used")
	apiServer := fs.String("api-server", "livepeer.com", "Server of the Livepeer API to be used")
	ingestStr := fs.String("ingest", "", "Ingest server info in JSON format including ingest and playback URLs. Should follow Livepeer API schema")
	analyzerServers := fs.String("analyzer-servers", "", "Comma-separated list of base URLs to connect for the Stream Health Analyzer API (defaults to --api-server)")
	fileArg := fs.String("file", "bbb_sunflower_1080p_30fps_normal_t02.mp4", "File to stream")
	vodImportUrl := fs.String("vod-import-url", "https://storage.googleapis.com/lp_testharness_assets/bbb_sunflower_1080p_30fps_normal_2min.mp4", "URL for VOD import")
	continuousTest := fs.Duration("continuous-test", 0, "Do continuous testing")
	useHttp := fs.Bool("http", false, "Do HTTP tests instead of RTMP")
	testMP4 := fs.Bool("mp4", false, "Download MP4 of recording")
	testStreamHealth := fs.Bool("stream-health", false, "Check stream health during test")
	testLive := fs.Bool("live", false, "Check Live workflow")
	testVod := fs.Bool("vod", false, "Check VOD workflow")
	transcodeBucketUrl := fs.String("transcode-bucket-url", "", "Object Store URL to test Transcode API in the format 's3+http(s)://<access-key-id>:<secret-access-key>@<endpoint>/<bucket>'")
	transcodeW3sProof := fs.String("transcode-w3s-proof", "", "Base64-encoded UCAN delegation proof to interact with web3.storage API")
	testTranscode := fs.Bool("transcode", false, "Check Transcode API workflow")
	catalystPipelineStrategy := fs.String("catalyst-pipeline-strategy", "", "Which catalyst pipeline strategy to use regarding. The appropriate values are defined by catalyst-api itself.")
	recordObjectStoreId := fs.String("record-object-store-id", "", "ID for the Object Store to use for recording storage. Forwarded to the streams created in the API")
	discordURL := fs.String("discord-url", "", "URL of Discord's webhook to send messages to Discord channel")
	discordUserName := fs.String("discord-user-name", "", "User name to use when sending messages to Discord")
	discordUsersToNotify := fs.String("discord-users", "", "Id's of users to notify in case of failure")
	pagerDutyIntegrationKey := fs.String("pagerduty-integration-key", "", "PagerDuty integration key")
	pagerDutyComponent := fs.String("pagerduty-component", "", "PagerDuty component")
	pagerDutyLowUrgency := fs.Bool("pagerduty-low-urgency", false, "Whether to send only low-urgency PagerDuty alerts")
	bind := fs.String("bind", "0.0.0.0:9090", "Address to bind metric server to")

	serfRPCAddr := fs.String("serf-rpc-addr", "", "Serf RPC address for fetching serf members")
	useSerf := fs.Bool("use-serf", false, "Use serf playback URLs")
	useRandomSerfMember := fs.Bool("random-serf-member", false, "Use a random member from serf member list")
	serfPullCount := fs.Int("pull-count", 1, "Number of serf nodes to pull playback from (requires `--use-serf`)")
	serfNodeCount := fs.Int("serf-node-count", 5, "Count of serf nodes when selecting nearest members")
	latitude := fs.Float64("latitude", 0, "latitude/geolocation of this record testing instance")
	longitude := fs.Float64("longitude", 0, "longitude/geolocation of this record testing instance")

	_ = fs.String("config", "", "config file (optional)")

	ff.Parse(fs, os.Args[1:],
		ff.WithConfigFileFlag("config"),
		ff.WithConfigFileParser(ff.PlainParser),
		ff.WithEnvVarPrefix("RT"),
		ff.WithEnvVarIgnoreCommas(true),
	)
	flag.CommandLine.Parse(nil)
	vFlag.Value.Set(*verbosity)

	hostName, _ := os.Hostname()
	fmt.Println("Recordtester version: " + model.Version)
	fmt.Printf("Compiler version: %s %s\n", runtime.Compiler, runtime.Version())
	fmt.Printf("Hostname %s OS %s IPs %v\n", hostName, runtime.GOOS, utils.GetIPs())
	fmt.Printf("Production: %v\n", model.Production)

	// Print out the args so that we know what the app is starting with after the various arg sources have been checked
	var args = []string{}
	fs.VisitAll(func(f *flag.Flag) {
		args = append(args, fmt.Sprintf("%s=%s", f.Name, f.Value))
	})
	fmt.Printf("Starting up with arguments: %s\n", strings.Join(args, ", "))

	if *version {
		return
	}
	metrics.InitCensus(hostName, model.Version, "recordtester")
	testers.IgnoreNoCodecError = true
	testers.IgnoreGaps = true
	testers.IgnoreTimeDrift = true
	testers.StartDelayBetweenGroups = 0
	model.ProfilesNum = 0

	if *fileArg == "" {
		fmt.Println("Should provide -file argument")
		os.Exit(1)
	}
	if *pauseDuration > 5*time.Minute {
		fmt.Println("Pause should be less than 5 min")
		os.Exit(1)
	}
	if *analyzerServers == "" {
		*analyzerServers = *apiServer
	}
	var fileName string
	var err error

	gctx, gcancel := context.WithCancel(context.Background()) // to be used as global parent context, in the future
	defer gcancel()

	if *testDuration == 0 {
		glog.Fatal("--test-dur should be specified")
	}
	if *apiToken == "" {
		glog.Fatal("--api-token should be specified")
	}
	if *useSerf && *serfRPCAddr == "" {
		glog.Fatal("--serf-rpc-addr needed with --use-serf option")
	}

	if fileName, err = utils.GetFile(*fileArg, strings.ReplaceAll(hostName, ".", "_")); err != nil {
		if err == utils.ErrNotFound {
			fmt.Printf("File %s not found\n", *fileArg)
		} else {
			fmt.Printf("Error getting file %s: %v\n", *fileArg, err)
		}
		os.Exit(1)
	}

	var ingest *api.Ingest
	if *ingestStr != "" {
		if err := json.Unmarshal([]byte(*ingestStr), &ingest); err != nil {
			glog.Fatalf("Error parsing --ingest argument: %v", err)
		}
	}

	serfMembers, err := getSerfMembers(*useSerf, *serfRPCAddr)
	if err != nil {
		glog.Fatalf("failed to process serf members: %v", err)
	}
	serfOptions := recordtester.SerfOptions{
		UseSerf:          *useSerf,
		SerfMembers:      getClosestSerfNodes(serfMembers, *serfNodeCount, *latitude, *longitude),
		RandomSerfMember: *useRandomSerfMember,
		SerfPullCount:    *serfPullCount,
	}

	var lapi *api.Client
	cleanup := func(fn, fa string) {
		if fn != fa {
			os.Remove(fn)
		}
	}
	exit := func(exitCode int, fn, fa string, err error) {
		cleanup(fn, fa)
		if err != context.Canceled {
			if err != nil {
				fmt.Printf("Error: %v\n", err)
			}
			if exitCode != 0 {
				glog.Errorf("Record test failed exitCode=%d err=%v", exitCode, err)
			}
		} else {
			exitCode = 0
		}
		os.Exit(exitCode)
	}

	lApiOpts := api.ClientOptions{
		Server:      *apiServer,
		AccessToken: *apiToken,
		Timeout:     8 * time.Second,
	}
	lapi, _ = api.NewAPIClientGeolocated(lApiOpts)
	glog.Infof("Choosen server: %s", lapi.GetServer())

	userAgent := model.AppName + "/" + model.Version
	lanalyzers := testers.AnalyzerByRegion{}
	for _, url := range strings.Split(*analyzerServers, ",") {
		lanalyzers[url] = client.NewAnalyzer(url, *apiToken, userAgent, 0)
	}

	exitc := make(chan os.Signal, 1)
	signal.Notify(exitc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	go func(fn, fa string) {
		<-exitc
		fmt.Println("Got Ctrl-C, cancelling")
		gcancel()
		cleanup(fn, fa)
		time.Sleep(2 * time.Second)
		// exit(0, fn, fa, nil)
	}(fileName, *fileArg)
	messenger.Init(gctx, *discordURL, *discordUserName, *discordUsersToNotify, "", "", "")

	rtOpts := recordtester.RecordTesterOptions{
		API:                 lapi,
		Analyzers:           lanalyzers,
		Ingest:              ingest,
		RecordObjectStoreId: *recordObjectStoreId,
		UseForceURL:         true,
		UseHTTP:             *useHttp,
		TestMP4:             *testMP4,
		TestStreamHealth:    *testStreamHealth,
	}
	if *sim > 1 {
		var testers []recordtester.IRecordTester
		var eses []int
		var wg sync.WaitGroup
		var es int
		var err error
		start := time.Now()

		for i := 0; i < *sim; i++ {
			rt := recordtester.NewRecordTester(gctx, rtOpts, serfOptions)
			eses = append(eses, 0)
			testers = append(testers, rt)
			wg.Add(1)
			go func(ii int) {
				les, lerr := rt.Start(fileName, *testDuration, *pauseDuration)
				glog.Infof("===> ii=%d les=%d lerr=%v", ii, les, lerr)
				eses[ii] = les
				if les != 0 {
					es = les
				}
				if lerr != nil {
					err = lerr
				}
				wg.Done()
			}(i)
			wait := time.Duration((3 + rand.Intn(5))) * time.Second
			time.Sleep(wait)
		}
		wg.Wait()
		var succ int
		for _, r := range eses {
			if r == 0 {
				succ++
			}
		}
		took := time.Since(start)
		glog.Infof("%d streams test ended in %s success %f%%", *sim, took, float64(succ)/float64(len(eses))*100.0)
		time.Sleep(1 * time.Hour)
		exit(es, fileName, *fileArg, err)
		return
	} else if *continuousTest > 0 {
		metricServer := server.NewMetricsServer()
		go metricServer.Start(gctx, *bind)
		eg, egCtx := errgroup.WithContext(gctx)
		if *testLive {
			eg.Go(func() error {
				crtOpts := recordtester.ContinuousRecordTesterOptions{
					PagerDutyIntegrationKey: *pagerDutyIntegrationKey,
					PagerDutyComponent:      *pagerDutyComponent,
					PagerDutyLowUrgency:     *pagerDutyLowUrgency,
					RecordTesterOptions:     rtOpts,
				}
				crt := recordtester.NewContinuousRecordTester(egCtx, crtOpts, serfOptions)
				return crt.Start(fileName, *testDuration, *pauseDuration, *continuousTest)
			})
		}
		if *testVod {
			vtOpts := common.TesterOptions{
				API:                      lapi,
				CatalystPipelineStrategy: *catalystPipelineStrategy,
			}
			eg.Go(func() error {
				cvtOpts := common.ContinuousTesterOptions{
					PagerDutyIntegrationKey: *pagerDutyIntegrationKey,
					PagerDutyComponent:      *pagerDutyComponent,
					PagerDutyLowUrgency:     *pagerDutyLowUrgency,
					TesterOptions:           vtOpts,
				}
				cvt := vodtester.NewContinuousVodTester(egCtx, cvtOpts)
				return cvt.Start(fileName, *vodImportUrl, *testDuration, *taskPollDuration, *continuousTest)
			})
		}
		if *testTranscode {
			ttOpts := common.TesterOptions{
				API:                      lapi,
				CatalystPipelineStrategy: *catalystPipelineStrategy,
			}
			eg.Go(func() error {
				cttOpts := common.ContinuousTesterOptions{
					PagerDutyIntegrationKey: *pagerDutyIntegrationKey,
					PagerDutyComponent:      *pagerDutyComponent,
					PagerDutyLowUrgency:     *pagerDutyLowUrgency,
					TesterOptions:           ttOpts,
				}
				ctt := transcodetester.NewContinuousTranscodeTester(egCtx, cttOpts)
				return ctt.Start(*fileArg, *transcodeBucketUrl, *transcodeW3sProof, *testDuration, *taskPollDuration, *continuousTest)
			})
		}
		if err := eg.Wait(); err != nil {
			glog.Warningf("Continuous test ended with err=%v", err)
		}
		exit(0, fileName, *fileArg, err)
		return
	}
	// just one stream
	rt := recordtester.NewRecordTester(gctx, rtOpts, serfOptions)
	es, err := rt.Start(fileName, *testDuration, *pauseDuration)
	exit(es, fileName, *fileArg, err)
}
