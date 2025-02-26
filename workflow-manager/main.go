// workflow-manager looks for batches to be processed from an input bucket,
// and schedules intake-batch tasks for intake-batch-workers to process those
// batches.
//
// It also looks for batches that have been intake'd, and schedules aggregate
// tasks.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/letsencrypt/prio-server/workflow-manager/batchpath"
	"github.com/letsencrypt/prio-server/workflow-manager/bucket"
	wfkubernetes "github.com/letsencrypt/prio-server/workflow-manager/kubernetes"
	"github.com/letsencrypt/prio-server/workflow-manager/monitor"
	"github.com/letsencrypt/prio-server/workflow-manager/task"
	"github.com/letsencrypt/prio-server/workflow-manager/utils"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/push"
	batchv1 "k8s.io/api/batch/v1"
)

// BuildInfo is generated at build time - see the Dockerfile.
var BuildInfo string

var k8sNS = flag.String("k8s-namespace", "", "Kubernetes namespace")
var isFirst = flag.Bool("is-first", false, "Whether this set of servers is \"first\", aka PHA servers")
var maxAge = flag.String("intake-max-age", "1h", "Max age (in Go duration format) for intake batches to be worth processing.")
var ingestorInput = flag.String("ingestor-input", "", "Bucket for input from ingestor (s3:// or gs://) (Required)")
var ingestorIdentity = flag.String("ingestor-identity", "", "Identity to use with ingestor bucket (Required for S3)")
var ownValidationInput = flag.String("own-validation-input", "", "Bucket for input of validation batches from self (s3:// or gs://) (required)")
var ownValidationIdentity = flag.String("own-validation-identity", "", "Identity to use with own validation bucket (Required for S3)")
var peerValidationInput = flag.String("peer-validation-input", "", "Bucket for input of validation batches from peer (s3:// or gs://) (required)")
var peerValidationIdentity = flag.String("peer-validation-identity", "", "Identity to use with peer validation bucket (Required for S3)")
var aggregationPeriod = flag.String("aggregation-period", "3h", "How much time each aggregation covers")
var gracePeriod = flag.String("grace-period", "1h", "Wait this amount of time after the end of an aggregation timeslice to run the aggregation")
var pushGateway = flag.String("push-gateway", "", "Set this to the gateway to use with prometheus. If left empty, workflow-manager will not use prometheus.")
var kubeconfigPath = flag.String("kube-config-path", "", "Path to the kubeconfig file to be used to authenticate to Kubernetes API")
var dryRun = flag.Bool("dry-run", false, "If set, no operations with side effects will be done.")
var taskQueueKind = flag.String("task-queue-kind", "", "Which task queue kind to use.")
var intakeTasksTopic = flag.String("intake-tasks-topic", "", "Name of the topic to which intake-batch tasks should be published")
var aggregateTasksTopic = flag.String("aggregate-tasks-topic", "", "Name of the topic to which aggregate tasks should be published")

// Arguments for gcp-pubsub task queue
var gcpPubSubCreatePubSubTopics = flag.Bool("gcp-pubsub-create-topics", false, "Whether to create the GCP PubSub topics used for intake and aggregation tasks.")
var gcpPubSubProjectID = flag.String("gcp-project-id", "", "Name of the GCP project ID being used for PubSub..")

// Arguments for aws-sns task queue
var awsSNSRegion = flag.String("aws-sns-region", "", "AWS region in which to publish to SNS topic")
var awsSNSIdentity = flag.String("aws-sns-identity", "", "AWS IAM ARN of the role to be assumed to publish to SNS topics")

// Define flags and arguments for other task queue implementations here.
// Argument names should be prefixed with the corresponding value of
// task-queue-kind to avoid conflicts.

// monitoring things
var (
	intakesStarted      monitor.CounterMonitor = &monitor.NoopCounter{}
	aggregationsStarted monitor.CounterMonitor = &monitor.NoopCounter{}
)

func main() {
	log.Printf("starting %s version %s. Args: %s", os.Args[0], BuildInfo, os.Args[1:])
	flag.Parse()

	if *pushGateway != "" {
		push.New(*pushGateway, "workflow-manager").Gatherer(prometheus.DefaultGatherer).Push()
		intakesStarted = promauto.NewCounter(prometheus.CounterOpts{
			Name: "intake_jobs_started",
			Help: "The number of intake-batch jobs successfully started",
		})

		aggregationsStarted = promauto.NewCounter(prometheus.CounterOpts{
			Name: "aggregation_jobs_started",
			Help: "The number of aggregate jobs successfully started",
		})
	}

	ownValidationBucket, err := bucket.New(*ownValidationInput, *ownValidationIdentity, *dryRun)
	if err != nil {
		log.Fatalf("--own-validation-input: %s", err)
	}
	peerValidationBucket, err := bucket.New(*peerValidationInput, *peerValidationIdentity, *dryRun)
	if err != nil {
		log.Fatalf("--peer-validation-input: %s", err)
	}
	intakeBucket, err := bucket.New(*ingestorInput, *ingestorIdentity, *dryRun)
	if err != nil {
		log.Fatalf("--ingestor-input: %s", err)
	}

	maxAgeParsed, err := time.ParseDuration(*maxAge)
	if err != nil {
		log.Fatalf("--max-age: %s", err)
	}

	gracePeriodParsed, err := time.ParseDuration(*gracePeriod)
	if err != nil {
		log.Fatalf("--grace-period: %s", err)
	}

	aggregationPeriodParsed, err := time.ParseDuration(*aggregationPeriod)
	if err != nil {
		log.Fatalf("--aggregation-time-slice: %s", err)
	}

	if *taskQueueKind == "" || *intakeTasksTopic == "" || *aggregateTasksTopic == "" {
		log.Fatalf("--task-queue-kind, --intake-tasks-topic and --aggregate-tasks-topic are required")
	}

	var intakeTaskEnqueuer task.Enqueuer
	var aggregationTaskEnqueuer task.Enqueuer

	switch *taskQueueKind {
	case "gcp-pubsub":
		if *gcpPubSubProjectID == "" {
			log.Fatal("--gcp-project-id is required for task-queue-kind=gcp-pubsub")
		}

		if *gcpPubSubCreatePubSubTopics {
			if err := task.CreatePubSubTopic(
				*gcpPubSubProjectID,
				*intakeTasksTopic,
			); err != nil {
				log.Fatalf("creating pubsub topic: %s", err)
			}
			if err := task.CreatePubSubTopic(
				*gcpPubSubProjectID,
				*aggregateTasksTopic,
			); err != nil {
				log.Fatalf("creating pubsub topic: %s", err)
			}
		}

		intakeTaskEnqueuer, err = task.NewGCPPubSubEnqueuer(
			*gcpPubSubProjectID,
			*intakeTasksTopic,
			*dryRun,
		)
		if err != nil {
			log.Fatal(err)
		}

		aggregationTaskEnqueuer, err = task.NewGCPPubSubEnqueuer(
			*gcpPubSubProjectID,
			*aggregateTasksTopic,
			*dryRun,
		)
		if err != nil {
			log.Fatal(err)
		}
	case "aws-sns":
		if *awsSNSRegion == "" {
			log.Fatal("--aws-sns-region is required for task-queue-kind=aws-sns")
		}

		intakeTaskEnqueuer, err = task.NewAWSSNSEnqueuer(
			*awsSNSRegion,
			*awsSNSIdentity,
			*intakeTasksTopic,
			*dryRun,
		)
		if err != nil {
			log.Fatal(err)
		}

		aggregationTaskEnqueuer, err = task.NewAWSSNSEnqueuer(
			*awsSNSRegion,
			*awsSNSIdentity,
			*aggregateTasksTopic,
			*dryRun,
		)
		if err != nil {
			log.Fatal(err)
		}
	// To implement a new task queue kind, add a case here. You should
	// initialize intakeTaskEnqueuer and aggregationTaskEnqueuer.
	default:
		log.Fatalf("unknown task queue kind %s", *taskQueueKind)
	}

	kubernetesClient, err := wfkubernetes.NewClient(*k8sNS, *kubeconfigPath, *dryRun)
	if err != nil {
		log.Fatal(err)
	}

	// Get a listing of all jobs in the namespace so the finished ones can be
	// reaped later on, and to avoid scheduling redudant work.
	existingJobs, err := kubernetesClient.ListJobs()
	if err != nil {
		log.Fatal(err)
	}

	intakeFiles, err := intakeBucket.ListFiles()
	if err != nil {
		log.Fatal(err)
	}

	ownValidationFiles, err := ownValidationBucket.ListFiles()
	if err != nil {
		log.Fatal(err)
	}

	peerValidationFiles, err := peerValidationBucket.ListFiles()
	if err != nil {
		log.Fatal(err)
	}

	scheduleTasks(scheduleTasksConfig{
		isFirst:                 *isFirst,
		clock:                   utils.DefaultClock(),
		intakeFiles:             intakeFiles,
		ownValidationFiles:      ownValidationFiles,
		peerValidationFiles:     peerValidationFiles,
		existingJobs:            existingJobs,
		intakeTaskEnqueuer:      intakeTaskEnqueuer,
		aggregationTaskEnqueuer: aggregationTaskEnqueuer,
		ownValidationBucket:     ownValidationBucket,
		maxAge:                  maxAgeParsed,
		aggregationPeriod:       aggregationPeriodParsed,
		gracePeriod:             gracePeriodParsed,
	})

	log.Print("done")
}

type scheduleTasksConfig struct {
	isFirst                                              bool
	clock                                                utils.Clock
	intakeFiles, ownValidationFiles, peerValidationFiles []string
	existingJobs                                         map[string]batchv1.Job
	intakeTaskEnqueuer, aggregationTaskEnqueuer          task.Enqueuer
	ownValidationBucket                                  bucket.TaskMarkerWriter
	maxAge, aggregationPeriod, gracePeriod               time.Duration
}

// scheduleTasks evaluates bucket contents and kubernetes cluster state to
// schedule new tasks or delete old jobs
func scheduleTasks(config scheduleTasksConfig) {
	intakeBatches, err := batchpath.ReadyBatches(config.intakeFiles, "batch")
	if err != nil {
		log.Fatal(err)
	}

	// Make a set of the tasks for which we have marker objects for efficient
	// lookup later.
	taskMarkers := map[string]struct{}{}
	for _, object := range config.ownValidationFiles {
		if !strings.HasPrefix(object, "task-markers/") {
			continue
		}
		taskMarkers[strings.TrimPrefix(object, "task-markers/")] = struct{}{}
	}

	currentIntakeBatches := withinInterval(intakeBatches, interval{
		begin: config.clock.Now().Add(-config.maxAge),
		end:   config.clock.Now().Add(24 * time.Hour),
	})
	log.Printf("skipping %d batches as too old", len(intakeBatches)-len(currentIntakeBatches))

	err = enqueueIntakeTasks(
		config.clock,
		currentIntakeBatches,
		config.maxAge,
		taskMarkers,
		config.existingJobs,
		config.ownValidationBucket,
		config.intakeTaskEnqueuer,
	)
	if err != nil {
		log.Fatal(err)
	}

	ownValidityInfix := fmt.Sprintf("validity_%d", utils.Index(config.isFirst))
	ownValidationBatches, err := batchpath.ReadyBatches(config.ownValidationFiles, ownValidityInfix)
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("found %d own validations", len(ownValidationBatches))

	peerValidityInfix := fmt.Sprintf("validity_%d", utils.Index(!config.isFirst))
	peerValidationBatches, err := batchpath.ReadyBatches(config.peerValidationFiles, peerValidityInfix)
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("found %d peer validations", len(peerValidationBatches))

	// Take the intersection of the sets of own validations and peer validations
	// to get the list of batches we can aggregate.
	// Go doesn't have sets, so we have to use a map[string]bool. We use the
	// batch ID as the key to the set, because batchPath is not a valid map key
	// type, and using a *batchPath wouldn't give us the lookup semantics we
	// want.
	ownValidationsSet := map[string]bool{}
	for _, ownValidationBatch := range ownValidationBatches {
		ownValidationsSet[ownValidationBatch.ID] = true
	}
	aggregationBatches := batchpath.List{}
	for _, peerValidationBatch := range peerValidationBatches {
		if _, ok := ownValidationsSet[peerValidationBatch.ID]; ok {
			aggregationBatches = append(aggregationBatches, peerValidationBatch)
		}
	}

	interval := aggregationInterval(config.clock, config.aggregationPeriod, config.gracePeriod)
	log.Printf("looking for batches to aggregate in interval %s", interval)
	aggregationBatches = withinInterval(aggregationBatches, interval)
	aggregationMap := groupByAggregationID(aggregationBatches)
	err = enqueueAggregationTasks(
		aggregationMap,
		interval,
		taskMarkers,
		config.existingJobs,
		config.ownValidationBucket,
		config.aggregationTaskEnqueuer,
	)
	if err != nil {
		log.Fatal(err)
	}

	// Ensure both task enqueuers have completed their asynchronous work before
	// allowing the process to exit
	config.intakeTaskEnqueuer.Stop()
	config.aggregationTaskEnqueuer.Stop()
}

// interval represents a half-open interval of time.
// It includes `begin` and excludes `end`.
type interval struct {
	begin time.Time
	end   time.Time
}

func (inter interval) String() string {
	return fmt.Sprintf("%s to %s", fmtTime(inter.begin), fmtTime(inter.end))
}

// fmtTime returns the input time in the same style expected by facilitator/lib.rs,
// currently "%Y/%m/%d/%H/%M"
func fmtTime(t time.Time) string {
	return t.Format("2006/01/02/15/04")
}

// jobNameForBatchPath generates a name for the Kubernetes job that will intake
// the provided batch. The name will incorporate the aggregation ID, batch UUID
// and batch timestamp while being a legal Kubernetes job name.
func intakeJobNameForBatchPath(path *batchpath.BatchPath) string {
	// Kubernetes job names must be valid DNS identifiers, which means they are
	// limited to 63 characters in length and also what characters they may
	// contain. Intake job names are like:
	// i-<aggregation name fragment>-<batch UUID fragment>-<batch timestamp>
	// The batch timestamp is 16 characters, and the 'i' and '-'es take up
	// another 4, leaving 43. We take the '-'es out of the UUID and use half of
	// it, hoping that this plus the date will provide enough entropy to avoid
	// collisions. Half a UUID is 16 characters, leaving 27 for the aggregation
	// ID fragment.
	// For example, we might get:
	// i-com-apple-EN-verylongnameth-0f0f0f0f0f0f0f0f-2006-01-02-15-04
	return fmt.Sprintf("i-%s-%s-%s",
		aggregationJobNameFragment(path.AggregationID, 27),
		strings.ReplaceAll(path.ID, "-", "")[:16],
		strings.ReplaceAll(fmtTime(path.Time), "/", "-"))
}

// aggregationJobNameFragment generates a job name-safe string from an
// aggregationID.
// Remove characters that aren't valid in DNS names, and also restrict
// the length so we don't go over the specified limit of characters.
func aggregationJobNameFragment(aggregationID string, maxLength int) string {
	re := regexp.MustCompile("[^A-Za-z0-9-]")
	idForJobName := re.ReplaceAllLiteralString(aggregationID, "-")
	if len(idForJobName) > maxLength {
		idForJobName = idForJobName[:maxLength]
	}
	idForJobName = strings.ToLower(idForJobName)
	return idForJobName
}

// aggregationInterval calculates the interval we want to run an aggregation for, if any.
// That is whatever interval is `gracePeriod` earlier than now and aligned on multiples
// of `aggregationPeriod` (relative to the zero time).
func aggregationInterval(clock utils.Clock, aggregationPeriod, gracePeriod time.Duration) interval {
	var output interval
	output.end = clock.Now().Add(-gracePeriod).Truncate(aggregationPeriod)
	output.begin = output.end.Add(-aggregationPeriod)
	return output
}

// withinInterval returns the subset of `batchPath`s that are within the given interval.
func withinInterval(batches batchpath.List, inter interval) batchpath.List {
	var output batchpath.List
	for _, bp := range batches {
		// We use Before twice rather than Before and after, because Before is <,
		// and After is >, but we are processing a half-open interval so we need
		// >= and <.
		if !bp.Time.Before(inter.begin) && bp.Time.Before(inter.end) {
			output = append(output, bp)
		}
	}
	return output
}

type aggregationMap map[string]batchpath.List

func groupByAggregationID(batches batchpath.List) aggregationMap {
	output := make(aggregationMap)
	for _, v := range batches {
		output[v.AggregationID] = append(output[v.AggregationID], v)
	}
	return output
}

func enqueueAggregationTasks(
	batchesByID aggregationMap,
	inter interval,
	taskMarkers map[string]struct{},
	existingJobs map[string]batchv1.Job,
	ownValidationBucket bucket.TaskMarkerWriter,
	enqueuer task.Enqueuer,
) error {
	if len(batchesByID) == 0 {
		log.Printf("no batches to aggregate")
		return nil
	}

	skippedDueToMarker := 0
	scheduled := 0

	for _, readyBatches := range batchesByID {
		aggregationID := readyBatches[0].AggregationID
		batches := []task.Batch{}

		batchCount := 0
		for _, batchPath := range readyBatches {
			batchCount++
			batches = append(batches, task.Batch{
				ID:   batchPath.ID,
				Time: task.Timestamp(batchPath.Time),
			})

			// All batches should have the same aggregation ID?
			if aggregationID != batchPath.AggregationID {
				return fmt.Errorf("found batch with aggregation ID %s, wanted %s", batchPath.AggregationID, aggregationID)
			}
		}

		aggregationTask := task.Aggregation{
			AggregationID:    aggregationID,
			AggregationStart: task.Timestamp(inter.begin),
			AggregationEnd:   task.Timestamp(inter.end),
			Batches:          batches,
		}

		if _, ok := taskMarkers[aggregationTask.Marker()]; ok {
			skippedDueToMarker++
			continue
		}

		taskName := fmt.Sprintf(
			"a-%s-%s",
			aggregationJobNameFragment(aggregationID, 30),
			strings.ReplaceAll(fmtTime(inter.begin), "/", "-"),
		)
		if _, ok := existingJobs[taskName]; ok {
			skippedDueToMarker++
			// If we made it here, a Kubernetes job for this aggregation
			// existed, but we did not find a marker for the task. The job was
			// most likely created by an older workflow-manager, so write out a
			// marker for this task, which makes it safe to reap the job when it
			// finishes.
			if err := ownValidationBucket.WriteTaskMarker(aggregationTask.Marker()); err != nil {
				return err
			}
			continue
		}

		log.Printf("scheduling aggregation task %s (interval %s) for aggregation ID %s over %d batches",
			taskName, inter, aggregationID, batchCount)
		scheduled++
		enqueuer.Enqueue(aggregationTask, func(err error) {
			if err != nil {
				log.Printf("failed to enqueue aggregation task: %s", err)
				return
			}

			// Write a marker to cloud storage to ensure we don't schedule
			// redundant tasks
			if err := ownValidationBucket.WriteTaskMarker(aggregationTask.Marker()); err != nil {
				log.Printf("failed to write aggregation task marker: %s", err)
			}

			aggregationsStarted.Inc()
		})
	}

	log.Printf("skipped %d aggregation tasks that already existed. Scheduled %d new aggregation tasks.",
		skippedDueToMarker, scheduled)

	return nil
}

func enqueueIntakeTasks(
	clock utils.Clock,
	readyBatches batchpath.List,
	ageLimit time.Duration,
	taskMarkers map[string]struct{},
	existingJobs map[string]batchv1.Job,
	ownValidationBucket bucket.TaskMarkerWriter,
	enqueuer task.Enqueuer,
) error {
	skippedDueToAge := 0
	skippedDueToMarker := 0
	scheduled := 0
	for _, batch := range readyBatches {
		age := clock.Now().Sub(batch.Time)
		if age > ageLimit {
			skippedDueToAge++
			continue
		}

		intakeTask := task.IntakeBatch{
			AggregationID: batch.AggregationID,
			BatchID:       batch.ID,
			Date:          task.Timestamp(batch.Time),
		}

		if _, ok := taskMarkers[intakeTask.Marker()]; ok {
			skippedDueToMarker++
			continue
		}

		taskName := intakeJobNameForBatchPath(batch)
		if _, ok := existingJobs[taskName]; ok {
			skippedDueToMarker++
			// If we made it here, a Kubernetes job for this intake task
			// existed, but we did not find a marker for the task. The job was
			// most likely created by an older workflow-manager, so write out a
			// marker for this task, which makes it safe to reap the job when it
			// finishes.
			if err := ownValidationBucket.WriteTaskMarker(intakeTask.Marker()); err != nil {
				return err
			}

			continue
		}

		log.Printf("scheduling intake task for batch %s", batch)
		scheduled++
		enqueuer.Enqueue(intakeTask, func(err error) {
			if err != nil {
				log.Printf("failed to enqueue intake task: %s", err)
				return
			}
			// Write a marker to cloud storage to ensure we don't schedule
			// redundant tasks
			if err := ownValidationBucket.WriteTaskMarker(intakeTask.Marker()); err != nil {
				log.Printf("failed to write intake task marker: %s", err)
				return
			}

			intakesStarted.Inc()
		})
	}

	log.Printf("skipped %d batches as too old, %d with existing tasks. Scheduled %d new intake tasks.",
		skippedDueToAge, skippedDueToMarker, scheduled)

	return nil
}
