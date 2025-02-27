// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

package googlepubsub

import (
	"context"
	"io/ioutil"
	"net/http"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/pubsub"
	"github.com/stretchr/testify/assert"
	"golang.org/x/sync/errgroup"
	"google.golang.org/api/iterator"

	"github.com/elastic/beats/filebeat/channel"
	"github.com/elastic/beats/filebeat/input"
	"github.com/elastic/beats/filebeat/util"
	"github.com/elastic/beats/libbeat/beat"
	"github.com/elastic/beats/libbeat/common"
	"github.com/elastic/beats/libbeat/logp"
	"github.com/elastic/beats/libbeat/tests/compose"
	"github.com/elastic/beats/libbeat/tests/resources"
)

const (
	emulatorProjectID    = "test-project-id"
	emulatorTopic        = "test-topic-foo"
	emulatorSubscription = "test-subscription-bar"
)

var once sync.Once

func testSetup(t *testing.T) *pubsub.Client {
	t.Helper()

	host := os.Getenv("PUBSUB_EMULATOR_HOST")
	if host == "" {
		t.Skip("PUBSUB_EMULATOR_HOST is not set in environment. You can start " +
			"the emulator with \"docker-compose up\" from the _meta directory. " +
			"The default address is PUBSUB_EMULATOR_HOST=localhost:8432")
	}

	if isInDockerIntegTestEnv() {
		// We're running inside out integration test environment so
		// make sure that that googlepubsub container is running.
		compose.EnsureUp(t, "googlepubsub")
	}

	once.Do(func() {
		logp.TestingSetup()
	})

	// Sanity check the emulator.
	resp, err := http.Get("http://" + host)
	if err != nil {
		t.Fatalf("pubsub emulator at %s is not healthy: %v", host, err)
	}
	_, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		t.Fatal("failed to read response", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pubsub emulator is not healthy, got status code %d", resp.StatusCode)
	}

	client, err := pubsub.NewClient(context.Background(), emulatorProjectID)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	resetPubSub(t, client)
	return client
}

func resetPubSub(t *testing.T, client *pubsub.Client) {
	ctx := context.Background()

	// Clear topics.
	topics := client.Topics(ctx)
	for {
		sub, err := topics.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}

		if err = sub.Delete(ctx); err != nil {
			t.Fatalf("failed to delete topic %v: %v", sub.ID(), err)
		}
	}

	// Clear subscriptions.
	subs := client.Subscriptions(ctx)
	for {
		sub, err := subs.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}

		if err = sub.Delete(ctx); err != nil {
			t.Fatalf("failed to delete subscription %v: %v", sub.ID(), err)
		}
	}
}

func createTopic(t *testing.T, client *pubsub.Client) {
	ctx := context.Background()
	topic := client.Topic(emulatorTopic)

	exists, err := topic.Exists(ctx)
	if err != nil {
		t.Fatalf("failed to check if topic exists: %v", err)
	}
	if !exists {
		if topic, err = client.CreateTopic(ctx, emulatorTopic); err != nil {
			t.Fatalf("failed to create the topic: %v", err)
		}
		t.Log("Topic created:", topic.ID())
	}
}

func publishMessages(t *testing.T, client *pubsub.Client, numMsgs int) []string {
	ctx := context.Background()
	topic := client.Topic(emulatorTopic)

	messageIDs := make([]string, numMsgs)
	for i := 0; i < numMsgs; i++ {
		result := topic.Publish(ctx, &pubsub.Message{
			Data: []byte(time.Now().UTC().Format(time.RFC3339Nano) + ": hello world " + strconv.Itoa(i)),
		})

		// Wait for message to publish and get assigned ID.
		id, err := result.Get(ctx)
		if err != nil {
			t.Fatal(err)
		}
		messageIDs[i] = id
	}
	t.Logf("Published %d messages to topic %v. ID range: [%v, %v]", len(messageIDs), topic.ID(), messageIDs[0], messageIDs[len(messageIDs)-1])
	return messageIDs
}

func createSubscription(t *testing.T, client *pubsub.Client) {
	ctx := context.Background()

	sub := client.Subscription(emulatorSubscription)
	exists, err := sub.Exists(ctx)
	if err != nil {
		t.Fatalf("failed to check if sub exists: %v", err)
	}
	if exists {
		return
	}

	sub, err = client.CreateSubscription(ctx, emulatorSubscription, pubsub.SubscriptionConfig{
		Topic: client.Topic(emulatorTopic),
	})
	if err != nil {
		t.Fatalf("failed to create subscription: %v", err)
	}
	t.Log("New subscription created:", sub.ID())
}

func ifNotDone(ctx context.Context, f func()) func() {
	return func() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		f()
	}
}

func defaultTestConfig() *common.Config {
	return common.MustNewConfigFrom(map[string]interface{}{
		"project_id": emulatorProjectID,
		"topic":      emulatorTopic,
		"subscription": map[string]interface{}{
			"name":   emulatorSubscription,
			"create": true,
		},
		"credentials_file": "NONE FOR EMULATOR TESTING",
	})
}

func isInDockerIntegTestEnv() bool {
	return os.Getenv("BEATS_DOCKER_INTEGRATION_TEST_ENV") != ""
}

func runTest(t *testing.T, cfg *common.Config, run func(client *pubsub.Client, input *pubsubInput, out *stubOutleter, t *testing.T)) {
	if !isInDockerIntegTestEnv() {
		// Don't test goroutines when using our compose.EnsureUp.
		defer resources.NewGoroutinesChecker().Check(t)
	}

	// Create pubsub client for setting up and communicating to emulator.
	client := testSetup(t)
	defer client.Close()

	// Simulate input.Context from Filebeat input runner.
	inputCtx := newInputContext()
	defer close(inputCtx.Done)

	// Stub outlet for receiving events generated by the input.
	eventOutlet := newStubOutlet()
	defer eventOutlet.Close()

	in, err := NewInput(cfg, eventOutlet.outlet, inputCtx)
	if err != nil {
		t.Fatal(err)
	}
	pubsubInput := in.(*pubsubInput)
	defer pubsubInput.Stop()

	run(client, pubsubInput, eventOutlet, t)
}

func newInputContext() input.Context {
	return input.Context{
		Done: make(chan struct{}),
	}
}

type stubOutleter struct {
	sync.Mutex
	cond   *sync.Cond
	done   bool
	Events []beat.Event
}

func newStubOutlet() *stubOutleter {
	o := &stubOutleter{}
	o.cond = sync.NewCond(o)
	return o
}

func (o *stubOutleter) outlet(_ *common.Config, _ *common.MapStrPointer) (channel.Outleter, error) {
	return o, nil
}

func (o *stubOutleter) waitForEvents(numEvents int) ([]beat.Event, bool) {
	o.Lock()
	defer o.Unlock()

	for len(o.Events) < numEvents && !o.done {
		o.cond.Wait()
	}

	size := numEvents
	if size >= len(o.Events) {
		size = len(o.Events)
	}

	out := make([]beat.Event, size)
	copy(out, o.Events)
	return out, len(out) == numEvents
}

func (o *stubOutleter) Close() error {
	o.Lock()
	defer o.Unlock()
	o.done = true
	return nil
}

func (o *stubOutleter) Done() <-chan struct{} { return nil }

func (o *stubOutleter) OnEvent(data *util.Data) bool {
	o.Lock()
	defer o.Unlock()
	o.Events = append(o.Events, data.Event)
	o.cond.Broadcast()
	return !o.done
}

// --- Test Cases

func TestTopicDoesNotExist(t *testing.T) {
	cfg := defaultTestConfig()

	runTest(t, cfg, func(client *pubsub.Client, input *pubsubInput, out *stubOutleter, t *testing.T) {
		err := input.run()
		if assert.Error(t, err) {
			assert.Contains(t, err.Error(), "failed to subscribe to pub/sub topic")
		}
	})
}

func TestSubscriptionDoesNotExistError(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.SetBool("subscription.create", -1, false)

	runTest(t, cfg, func(client *pubsub.Client, input *pubsubInput, out *stubOutleter, t *testing.T) {
		createTopic(t, client)

		err := input.run()
		if assert.Error(t, err) {
			assert.Contains(t, err.Error(), "no subscription exists and 'subscription.create' is not enabled")
		}
	})
}

func TestSubscriptionExists(t *testing.T) {
	cfg := defaultTestConfig()

	runTest(t, cfg, func(client *pubsub.Client, input *pubsubInput, out *stubOutleter, t *testing.T) {
		createTopic(t, client)
		createSubscription(t, client)
		publishMessages(t, client, 5)

		var group errgroup.Group
		group.Go(input.run)

		time.AfterFunc(10*time.Second, func() { out.Close() })
		events, ok := out.waitForEvents(5)
		if !ok {
			t.Fatalf("Expected 5 events, but got %d.", len(events))
		}
		input.Stop()

		if err := group.Wait(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestSubscriptionCreate(t *testing.T) {
	cfg := defaultTestConfig()

	runTest(t, cfg, func(client *pubsub.Client, input *pubsubInput, out *stubOutleter, t *testing.T) {
		createTopic(t, client)

		group, ctx := errgroup.WithContext(context.Background())
		group.Go(input.run)

		time.AfterFunc(1*time.Second, ifNotDone(ctx, func() { publishMessages(t, client, 5) }))
		time.AfterFunc(10*time.Second, func() { out.Close() })

		events, ok := out.waitForEvents(5)
		if !ok {
			t.Fatalf("Expected 5 events, but got %d.", len(events))
		}
		input.Stop()

		if err := group.Wait(); err != nil {
			t.Fatal(err)
		}
	})
}
