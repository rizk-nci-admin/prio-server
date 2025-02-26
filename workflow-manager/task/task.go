// package task contains representations of tasks that are sent to facilitator
// workers by workflow-manager. A _task_ is a work item at the data share
// processor application layer (i.e., intake of a batch or aggregation of many
// batches); its name is chosen to distinguish from Kubernetes-level _jobs_.
package task

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	leaws "github.com/letsencrypt/prio-server/workflow-manager/aws"
	"github.com/letsencrypt/prio-server/workflow-manager/utils"

	"cloud.google.com/go/pubsub"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/sns"
)

// Timestamp is an alias to time.Time with a custom JSON marshaler that
// marshals the time to UTC, with minute precision, in the format
// "2006/01/02/15/04"
type Timestamp time.Time

func (t Timestamp) MarshalJSON() ([]byte, error) {
	return json.Marshal(t.String())
}

func (t *Timestamp) stringWithFormat(format string) string {
	asTime := (*time.Time)(t)
	return asTime.Format(format)
}

func (t *Timestamp) String() string {
	return t.stringWithFormat("2006/01/02/15/04")
}

// Returns the representation of the timestamp as it should be incorporated into
// a task marker
func (t *Timestamp) MarkerString() string {
	return t.stringWithFormat("2006-01-02-15-04")
}

// Task is a task that can be enqueued into an Enqueuer
type Task interface {
	// Marker returns the name that should be used when writing out a marker for
	// this task
	Marker() string
}

// Aggregation represents an aggregation task
type Aggregation struct {
	// AggregationID is the identifier for the aggregation
	AggregationID string `json:"aggregation-id"`
	// AggregationStart is the start of the range of time covered by the
	// aggregation
	AggregationStart Timestamp `json:"aggregation-start"`
	// AggregationEnd is the end of the range of time covered by the aggregation
	AggregationEnd Timestamp `json:"aggregation-end"`
	// Batches is the list of batch ID date pairs of the batches aggregated by
	// this task
	Batches []Batch `json:"batches"`
}

func (a Aggregation) Marker() string {
	return fmt.Sprintf(
		"aggregate-%s-%s-%s",
		a.AggregationID,
		a.AggregationStart.MarkerString(),
		a.AggregationEnd.MarkerString(),
	)
}

// Batch represents a batch included in an aggregation task
type Batch struct {
	// ID is the batch ID. Typically a UUID.
	ID string `json:"id"`
	// Time is the timestamp on the batch
	Time Timestamp `json:"time"`
}

type IntakeBatch struct {
	// AggregationID is the identifier for the aggregaton
	AggregationID string `json:"aggregation-id"`
	// BatchID is the identifier of the batch. Typically a UUID.
	BatchID string `json:"batch-id"`
	// Date is the timestamp on the batch
	Date Timestamp `json:"date"`
}

func (i IntakeBatch) Marker() string {
	return fmt.Sprintf("intake-%s-%s-%s", i.AggregationID, i.Date.MarkerString(), i.BatchID)
}

// Enqueuer allows enqueuing tasks.
type Enqueuer interface {
	// Enqueue enqueues a task to be executed later. The provided completion
	// function will be invoked once the task is either successfully enqueued or
	// some unretryable error has occurred. A call to Stop() will not return
	// until completion functions passed to any and all calls to Enqueue() have
	// returned.
	Enqueue(task Task, completion func(error))
	// Stop blocks until all tasks passed to Enqueue() have been enqueued in the
	// underlying system, and all completion functions pased to Enqueue() have
	// returned, and so it is safe to exit the program without losing any tasks.
	Stop()
}

// CreatePubSubTopic creates a PubSub topic with the provided ID, as well as a
// subscription with the same ID that can later be used by a facilitator.
// Returns error on failure.
func CreatePubSubTopic(project string, topicID string) error {
	ctx, cancel := utils.ContextWithTimeout()
	defer cancel()

	client, err := pubsub.NewClient(ctx, project)
	if err != nil {
		return fmt.Errorf("pubsub.newClient: %w", err)
	}

	topic, err := client.CreateTopic(ctx, topicID)
	if err != nil {
		return fmt.Errorf("pubsub.CreateTopic: %w", err)
	}

	tenMinutes, _ := time.ParseDuration("10m")

	subscriptionConfig := pubsub.SubscriptionConfig{
		Topic:            topic,
		AckDeadline:      tenMinutes,
		ExpirationPolicy: time.Duration(0), // never expire
	}
	if _, err := client.CreateSubscription(ctx, topicID, subscriptionConfig); err != nil {
		return fmt.Errorf("pubsub.CreateSubscription: %w", err)
	}

	return nil
}

// GCPPubSubEnqueuer implements Enqueuer using GCP PubSub
type GCPPubSubEnqueuer struct {
	topic     *pubsub.Topic
	waitGroup sync.WaitGroup
	dryRun    bool
}

// NewGCPPubSubEnqueuer creates a task enqueuer for a given project and topic
// in GCP PubSub. If dryRun is true, no tasks will actually be enqueued. Clients
// should re-use a single instance as much as possible to enable batching of
// publish requests.
func NewGCPPubSubEnqueuer(project string, topicID string, dryRun bool) (*GCPPubSubEnqueuer, error) {
	ctx, cancel := utils.ContextWithTimeout()
	defer cancel()

	client, err := pubsub.NewClient(ctx, project)
	if err != nil {
		return nil, fmt.Errorf("pubsub.NewClient: %w", err)
	}

	return &GCPPubSubEnqueuer{
		topic:  client.Topic(topicID),
		dryRun: dryRun,
	}, nil
}

func (e *GCPPubSubEnqueuer) Enqueue(task Task, completion func(error)) {
	e.waitGroup.Add(1)
	go func(task Task) {
		defer e.waitGroup.Done()
		jsonTask, err := json.Marshal(task)
		if err != nil {
			completion(fmt.Errorf("marshaling task to JSON: %w", err))
			return
		}

		if e.dryRun {
			log.Printf("dry run, not enqueuing task")
			completion(nil)
			return
		}

		// Publish() returns immediately, giving us a handle to the result that we
		// can block on to see if publishing succeeded. The PubSub client
		// automatically retries for us, so we just keep the handle so the caller
		// can do whatever they need to after successful publication and we can
		// block in Stop() until all tasks have been enqueued
		ctx, cancel := utils.ContextWithTimeout()
		defer cancel()
		res := e.topic.Publish(ctx, &pubsub.Message{Data: jsonTask})
		if _, err := res.Get(ctx); err != nil {
			completion(fmt.Errorf("Failed to publish task %+v: %w", task, err))
		}

		completion(nil)
	}(task)
}

func (e *GCPPubSubEnqueuer) Stop() {
	e.waitGroup.Wait()
}

// AWSSNSEnqueuer implements Enqueuer using AWS SNS
type AWSSNSEnqueuer struct {
	service   *sns.SNS
	topicARN  string
	waitGroup sync.WaitGroup
	dryRun    bool
}

func NewAWSSNSEnqueuer(region, identity, topicARN string, dryRun bool) (*AWSSNSEnqueuer, error) {
	session, config, err := leaws.ClientConfig(region, identity)
	if err != nil {
		return nil, err
	}

	return &AWSSNSEnqueuer{
		service:  sns.New(session, config),
		topicARN: topicARN,
		dryRun:   dryRun,
	}, nil
}

func (e *AWSSNSEnqueuer) Enqueue(task Task, completion func(error)) {
	// sns.Publish() blocks until the message has been saved by SNS, so no need
	// to asynchronously handle completion. However we still want to maintain
	// the guarantee that Stop() will block until all pending calls to Enqueue()
	// complete, so we still use a waitgroup.
	e.waitGroup.Add(1)
	defer e.waitGroup.Done()

	jsonTask, err := json.Marshal(task)
	if err != nil {
		completion(fmt.Errorf("marshaling task to JSON: %w", err))
		return
	}

	if e.dryRun {
		log.Printf("dry run, not enqueuing task")
		completion(nil)
		return
	}
	// There's nothing in the PublishOutput we care about, so we discard it.
	_, err = e.service.Publish(&sns.PublishInput{
		TopicArn: aws.String(e.topicARN),
		Message:  aws.String(string(jsonTask)),
	})
	if err != nil {
		completion(fmt.Errorf("failed to publish task %+v: %w", task, err))
		return
	}

	completion(nil)
}

func (e *AWSSNSEnqueuer) Stop() {
	e.waitGroup.Wait()
}
