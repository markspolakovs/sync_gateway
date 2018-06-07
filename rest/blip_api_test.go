package rest

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/couchbase/go-blip"
	"github.com/couchbase/sync_gateway/base"
	"github.com/couchbase/sync_gateway/db"
	"github.com/couchbaselabs/go.assert"
)

// This test performs the following steps against the Sync Gateway passive blip replicator:
//
// - Setup
//   - Create an httptest server listening on a port that wraps the Sync Gateway Admin Handler
//   - Make a BLIP/Websocket client connection to Sync Gateway
// - Test
//   - Verify Sync Gateway will accept the doc revision that is about to be sent
//   - Send the doc revision in a rev request
//   - Call changes endpoint and verify that it knows about the revision just sent
//   - Call subChanges api and make sure we get expected changes back
//
// Replication Spec: https://github.com/couchbase/couchbase-lite-core/wiki/Replication-Protocol#proposechanges
func TestBlipPushRevisionInspectChanges(t *testing.T) {

	defer base.SetUpTestLogging(base.LevelInfo, base.KeyHTTP|base.KeySync|base.KeySyncMsg)()

	bt, err := NewBlipTester()
	assertNoError(t, err, "Error creating BlipTester")
	defer bt.Close()

	// Verify Sync Gateway will accept the doc revision that is about to be sent
	var changeList [][]interface{}
	changesRequest := blip.NewRequest()
	changesRequest.SetProfile("changes")
	changesRequest.SetBody([]byte(`[["1", "foo", "1-abc", false]]`)) // [sequence, docID, revID]
	sent := bt.sender.Send(changesRequest)
	assert.True(t, sent)
	changesResponse := changesRequest.Response()
	assert.Equals(t, changesResponse.SerialNumber(), changesRequest.SerialNumber())
	body, err := changesResponse.Body()
	assertNoError(t, err, "Error reading changes response body")
	err = json.Unmarshal(body, &changeList)
	assertNoError(t, err, "Error unmarshalling response body")
	assert.Equals(t, len(changeList), 1) // Should be 1 row, corresponding to the single doc that was queried in changes
	changeRow := changeList[0]
	assert.Equals(t, len(changeRow), 0) // Should be empty, meaning the server is saying it doesn't have the revision yet

	// Send the doc revision in a rev request
	_, _, revResponse, err := bt.SendRev(
		"foo",
		"1-abc",
		[]byte(`{"key": "val"}`),
		blip.Properties{},
	)
	assert.Equals(t, err, nil)

	_, err = revResponse.Body()
	assertNoError(t, err, "Error unmarshalling response body")

	// Call changes with a hypothetical new revision, assert that it returns last pushed revision
	var changeList2 [][]interface{}
	changesRequest2 := blip.NewRequest()
	changesRequest2.SetProfile("changes")
	changesRequest2.SetBody([]byte(`[["2", "foo", "2-xyz", false]]`)) // [sequence, docID, revID]
	sent2 := bt.sender.Send(changesRequest2)
	assert.True(t, sent2)
	changesResponse2 := changesRequest2.Response()
	assert.Equals(t, changesResponse2.SerialNumber(), changesRequest2.SerialNumber())
	body2, err := changesResponse2.Body()
	assertNoError(t, err, "Error reading changes response body")
	err = json.Unmarshal(body2, &changeList2)
	assertNoError(t, err, "Error unmarshalling response body")
	assert.Equals(t, len(changeList2), 1) // Should be 1 row, corresponding to the single doc that was queried in changes
	changeRow2 := changeList2[0]
	assert.Equals(t, len(changeRow2), 1) // Should have 1 item in row, which is the rev id of the previous revision pushed
	assert.Equals(t, changeRow2[0], "1-abc")

	// Call subChanges api and make sure we get expected changes back
	receivedChangesRequestWg := sync.WaitGroup{}

	// When this test sends subChanges, Sync Gateway will send a changes request that must be handled
	bt.blipContext.HandlerForProfile["changes"] = func(request *blip.Message) {

		log.Printf("got changes message: %+v", request)
		body, err := request.Body()
		log.Printf("changes body: %v, err: %v", string(body), err)

		if string(body) != "null" {

			// Expected changes body: [[1,"foo","1-abc"]]
			changeListReceived := [][]interface{}{}
			err = json.Unmarshal(body, &changeListReceived)
			assertNoError(t, err, "Error unmarshalling changes received")
			assert.Equals(t, len(changeListReceived), 1)
			change := changeListReceived[0] // [1,"foo","1-abc"]
			assert.Equals(t, len(change), 3)
			assert.Equals(t, change[0].(float64), float64(1)) // Expect sequence to be 1, since first item in DB
			assert.Equals(t, change[1], "foo")                // Doc id of pushed rev
			assert.Equals(t, change[2], "1-abc")              // Rev id of pushed rev

		}

		if !request.NoReply() {
			// Send an empty response to avoid the Sync: Invalid response to 'changes' message
			response := request.Response()
			emptyResponseVal := []interface{}{}
			emptyResponseValBytes, err := json.Marshal(emptyResponseVal)
			assertNoError(t, err, "Error marshalling response")
			response.SetBody(emptyResponseValBytes)
		}

		receivedChangesRequestWg.Done()

	}

	// Send subChanges to subscribe to changes, which will cause the "changes" profile handler above to be called back
	subChangesRequest := blip.NewRequest()
	subChangesRequest.SetProfile("subChanges")
	subChangesRequest.Properties["continuous"] = "true"
	sent = bt.sender.Send(subChangesRequest)
	assert.True(t, sent)
	receivedChangesRequestWg.Add(1)
	subChangesResponse := subChangesRequest.Response()
	assert.Equals(t, subChangesResponse.SerialNumber(), subChangesRequest.SerialNumber())

	// Also expect the "changes" profile handler above to be called back again with an empty request that
	// will be ignored since body will be "null"
	receivedChangesRequestWg.Add(1)

	// Wait until we got the expected callback on the "changes" profile handler
	timeoutErr := WaitWithTimeout(&receivedChangesRequestWg, time.Second*5)
	assertNoError(t, timeoutErr, "Timed out waiting")
}

// Start subChanges w/ continuous=true, batchsize=20
// Make several updates
// Wait until we get the expected updates
func TestContinuousChangesSubscription(t *testing.T) {

	defer base.SetUpTestLogging(base.LevelInfo, base.KeyHTTP|base.KeySync|base.KeySyncMsg)()

	bt, err := NewBlipTester()
	assertNoError(t, err, "Error creating BlipTester")
	defer bt.Close()

	// Counter/Waitgroup to help ensure that all callbacks on continuous changes handler are received
	receivedChangesWg := sync.WaitGroup{}

	// When this test sends subChanges, Sync Gateway will send a changes request that must be handled
	lastReceivedSeq := float64(0)
	var numbatchesReceived int32
	bt.blipContext.HandlerForProfile["changes"] = func(request *blip.Message) {

		body, err := request.Body()

		if string(body) != "null" {

			atomic.AddInt32(&numbatchesReceived, 1)

			// Expected changes body: [[1,"foo","1-abc"]]
			changeListReceived := [][]interface{}{}
			err = json.Unmarshal(body, &changeListReceived)
			assertNoError(t, err, "Error unmarshalling changes received")

			for _, change := range changeListReceived {

				// The change should have three items in the array
				// [1,"foo","1-abc"]
				assert.Equals(t, len(change), 3)

				// Make sure sequence numbers are monotonically increasing
				receivedSeq := change[0].(float64)
				assert.True(t, receivedSeq > lastReceivedSeq)
				lastReceivedSeq = receivedSeq

				// Verify doc id and rev id have expected vals
				docId := change[1].(string)
				assert.True(t, strings.HasPrefix(docId, "foo"))
				assert.Equals(t, change[2], "1-abc") // Rev id of pushed rev

				receivedChangesWg.Done()
			}

		} else {

			receivedChangesWg.Done()

		}

		if !request.NoReply() {
			// Send an empty response to avoid the Sync: Invalid response to 'changes' message
			response := request.Response()
			emptyResponseVal := []interface{}{}
			emptyResponseValBytes, err := json.Marshal(emptyResponseVal)
			assertNoError(t, err, "Error marshalling response")
			response.SetBody(emptyResponseValBytes)
		}

	}

	// Increment waitgroup since just the act of subscribing to continuous changes will cause
	// the callback changes handler to be invoked with an initial change w/ empty body, signaling that
	// all of the changes have been sent (eg, there are no changes to send)
	receivedChangesWg.Add(1)

	// Send subChanges to subscribe to changes, which will cause the "changes" profile handler above to be called back
	subChangesRequest := blip.NewRequest()
	subChangesRequest.SetProfile("subChanges")
	subChangesRequest.Properties["continuous"] = "true"
	subChangesRequest.Properties["batch"] = "10" // default batch size is 200, lower this to 10 to make sure we get multiple batches
	subChangesRequest.SetCompressed(false)
	sent := bt.sender.Send(subChangesRequest)
	assert.True(t, sent)
	subChangesResponse := subChangesRequest.Response()
	assert.Equals(t, subChangesResponse.SerialNumber(), subChangesRequest.SerialNumber())

	for i := 1; i < 1500; i++ {
		//// Add a change: Send an unsolicited doc revision in a rev request
		receivedChangesWg.Add(1)
		_, _, revResponse, err := bt.SendRev(
			fmt.Sprintf("foo-%d", i),
			"1-abc",
			[]byte(`{"key": "val"}`),
			blip.Properties{},
		)
		assert.Equals(t, err, nil)

		_, err = revResponse.Body()
		assertNoError(t, err, "Error unmarshalling response body")

	}

	// Wait until all expected changes are received by change handler
	// receivedChangesWg.Wait()
	timeoutErr := WaitWithTimeout(&receivedChangesWg, time.Second*5)
	assertNoError(t, timeoutErr, "Timed out waiting for all changes.")

	// Since batch size was set to 10, and 15 docs were added, expect at _least_ 2 batches
	numBatchesReceivedSnapshot := atomic.LoadInt32(&numbatchesReceived)
	assert.True(t, numBatchesReceivedSnapshot >= 2)

}

// Make several updates
// Start subChanges w/ continuous=false, batchsize=20
// Validate we get the expected updates and changes ends
func TestBlipOneShotChangesSubscription(t *testing.T) {

	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	defer base.SetUpTestLogging(base.LevelInfo, base.KeyHTTP|base.KeySync|base.KeySyncMsg)()

	bt, err := NewBlipTester()
	assertNoError(t, err, "Error creating BlipTester")
	defer bt.Close()

	// Counter/Waitgroup to help ensure that all callbacks on continuous changes handler are received
	receivedChangesWg := sync.WaitGroup{}
	receivedCaughtUpChange := false

	// Build set of docids
	docIdsReceived := make(map[string]bool)
	for i := 1; i < 105; i++ {
		docIdsReceived[fmt.Sprintf("preOneShot-%d", i)] = false
	}

	// When this test sends subChanges, Sync Gateway will send a changes request that must be handled
	lastReceivedSeq := float64(0)
	var numbatchesReceived int32
	bt.blipContext.HandlerForProfile["changes"] = func(request *blip.Message) {

		body, err := request.Body()

		if string(body) != "null" {

			atomic.AddInt32(&numbatchesReceived, 1)

			// Expected changes body: [[1,"foo","1-abc"]]
			changeListReceived := [][]interface{}{}
			err = json.Unmarshal(body, &changeListReceived)
			assertNoError(t, err, "Error unmarshalling changes received")

			for _, change := range changeListReceived {

				// The change should have three items in the array
				// [1,"foo","1-abc"]
				assert.Equals(t, len(change), 3)

				// Make sure sequence numbers are monotonically increasing
				receivedSeq := change[0].(float64)
				assert.True(t, receivedSeq > lastReceivedSeq)
				lastReceivedSeq = receivedSeq

				// Verify doc id and rev id have expected vals
				docId := change[1].(string)
				assert.True(t, strings.HasPrefix(docId, "preOneShot"))
				assert.Equals(t, change[2], "1-abc") // Rev id of pushed rev
				docIdsReceived[docId] = true
				receivedChangesWg.Done()
			}

		} else {
			receivedCaughtUpChange = true
			receivedChangesWg.Done()

		}

		if !request.NoReply() {
			// Send an empty response to avoid the Sync: Invalid response to 'changes' message
			response := request.Response()
			emptyResponseVal := []interface{}{}
			emptyResponseValBytes, err := json.Marshal(emptyResponseVal)
			assertNoError(t, err, "Error marshalling response")
			response.SetBody(emptyResponseValBytes)
		}

	}

	// Increment waitgroup to account for the expected 'caught up' nil changes entry.
	receivedChangesWg.Add(1)

	// Add documents
	for docID, _ := range docIdsReceived {
		//// Add a change: Send an unsolicited doc revision in a rev request
		_, _, revResponse, err := bt.SendRev(
			docID,
			"1-abc",
			[]byte(`{"key": "val"}`),
			blip.Properties{},
		)
		assert.Equals(t, err, nil)
		_, err = revResponse.Body()
		assertNoError(t, err, "Error unmarshalling response body")
		receivedChangesWg.Add(1)
	}

	// Wait for documents to be processed and available for changes
	expectedSequence := uint64(104)
	bt.restTester.WaitForSequence(expectedSequence)

	// Send subChanges to subscribe to changes, which will cause the "changes" profile handler above to be called back
	subChangesRequest := blip.NewRequest()
	subChangesRequest.SetProfile("subChanges")
	subChangesRequest.Properties["continuous"] = "false"
	subChangesRequest.Properties["batch"] = "10" // default batch size is 200, lower this to 10 to make sure we get multiple batches
	subChangesRequest.SetCompressed(false)
	sent := bt.sender.Send(subChangesRequest)
	assert.True(t, sent)
	subChangesResponse := subChangesRequest.Response()
	assert.Equals(t, subChangesResponse.SerialNumber(), subChangesRequest.SerialNumber())

	// Wait until all expected changes are received by change handler
	// receivedChangesWg.Wait()
	timeoutErr := WaitWithTimeout(&receivedChangesWg, time.Second*60)
	assertNoError(t, timeoutErr, "Timed out waiting for all changes.")

	// Since batch size was set to 10, and 15 docs were added, expect at _least_ 2 batches
	numBatchesReceivedSnapshot := atomic.LoadInt32(&numbatchesReceived)
	assert.True(t, numBatchesReceivedSnapshot >= 2)

	// Validate all expected documents were received.
	for docID, received := range docIdsReceived {
		if !received {
			t.Errorf("Did not receive expected doc %s in changes", docID)
		}
	}

	// Validate that the 'caught up' message was sent
	assert.True(t, receivedCaughtUpChange)

	// Create a few more changes, validate that they aren't sent (subChanges has been closed).
	// Validated by the prefix matching in the subChanges callback, as well as waitgroup check below.
	for i := 0; i < 5; i++ {
		//// Add a change: Send an unsolicited doc revision in a rev request
		_, _, revResponse, err := bt.SendRev(
			fmt.Sprintf("postOneShot_%d", i),
			"1-abc",
			[]byte(`{"key": "val"}`),
			blip.Properties{},
		)
		assert.Equals(t, err, nil)
		_, err = revResponse.Body()
		assertNoError(t, err, "Error unmarshalling response body")
		receivedChangesWg.Add(1)
	}

	// Wait long enough to ensure the changes aren't being sent
	expectedTimeoutErr := WaitWithTimeout(&receivedChangesWg, time.Second*3)
	if expectedTimeoutErr == nil {
		t.Errorf("Received additional changes after one-shot should have been closed.")
	}
}

// Test subChanges w/ docID filter
func TestBlipSubChangesDocIDFilter(t *testing.T) {

	defer base.SetUpTestLogging(base.LevelInfo, base.KeyHTTP|base.KeySync|base.KeySyncMsg)()

	bt, err := NewBlipTester()
	assertNoError(t, err, "Error creating BlipTester")
	defer bt.Close()

	// Counter/Waitgroup to help ensure that all callbacks on continuous changes handler are received
	receivedChangesWg := sync.WaitGroup{}
	receivedCaughtUpChange := false

	// Build set of docids
	docIDsSent := make([]string, 0)
	docIDsExpected := make([]string, 0)
	docIDsReceived := make(map[string]bool)
	for i := 1; i <= 100; i++ {
		docID := fmt.Sprintf("docIDFiltered-%d", i)
		docIDsSent = append(docIDsSent, docID)
		// Filter to every 5th doc
		if i%5 == 0 {
			docIDsExpected = append(docIDsExpected, docID)
			docIDsReceived[docID] = false
		}
	}

	// When this test sends subChanges, Sync Gateway will send a changes request that must be handled
	lastReceivedSeq := float64(0)
	var numbatchesReceived int32
	bt.blipContext.HandlerForProfile["changes"] = func(request *blip.Message) {

		body, err := request.Body()

		if string(body) != "null" {

			atomic.AddInt32(&numbatchesReceived, 1)

			// Expected changes body: [[1,"foo","1-abc"]]
			changeListReceived := [][]interface{}{}
			err = json.Unmarshal(body, &changeListReceived)
			assertNoError(t, err, "Error unmarshalling changes received")

			for _, change := range changeListReceived {

				// The change should have three items in the array
				// [1,"foo","1-abc"]
				assert.Equals(t, len(change), 3)

				// Make sure sequence numbers are monotonically increasing
				receivedSeq := change[0].(float64)
				assert.True(t, receivedSeq > lastReceivedSeq)
				lastReceivedSeq = receivedSeq

				// Verify doc id and rev id have expected vals
				docId := change[1].(string)
				assert.True(t, strings.HasPrefix(docId, "docIDFiltered"))
				assert.Equals(t, change[2], "1-abc") // Rev id of pushed rev
				log.Printf("Changes got docID: %s", docId)

				// Ensure we only receive expected docs
				_, isExpected := docIDsReceived[docId]
				if !isExpected {
					t.Errorf(fmt.Sprintf("Received unexpected docId: %s", docId))
				} else {
					// Add to received set, to ensure we get all expected docs
					docIDsReceived[docId] = true
					receivedChangesWg.Done()
				}

			}

		} else {
			receivedCaughtUpChange = true
			receivedChangesWg.Done()

		}

		if !request.NoReply() {
			// Send an empty response to avoid the Sync: Invalid response to 'changes' message
			response := request.Response()
			emptyResponseVal := []interface{}{}
			emptyResponseValBytes, err := json.Marshal(emptyResponseVal)
			assertNoError(t, err, "Error marshalling response")
			response.SetBody(emptyResponseValBytes)
		}

	}

	// Increment waitgroup to account for the expected 'caught up' nil changes entry.
	receivedChangesWg.Add(1)

	// Add documents
	for _, docID := range docIDsSent {
		//// Add a change: Send an unsolicited doc revision in a rev request
		_, _, revResponse, err := bt.SendRev(
			docID,
			"1-abc",
			[]byte(`{"key": "val"}`),
			blip.Properties{},
		)
		assert.Equals(t, err, nil)
		_, err = revResponse.Body()
		assertNoError(t, err, "Error unmarshalling response body")
	}
	receivedChangesWg.Add(len(docIDsExpected))

	// Wait for documents to be processed and available for changes
	// 105 docs +
	expectedSequence := uint64(100)
	bt.restTester.WaitForSequence(expectedSequence)

	// TODO: Attempt a subChanges w/ continuous=true and docID filter

	// Send subChanges to subscribe to changes, which will cause the "changes" profile handler above to be called back
	subChangesRequest := blip.NewRequest()
	subChangesRequest.SetProfile("subChanges")
	subChangesRequest.Properties["continuous"] = "false"
	subChangesRequest.Properties["batch"] = "10" // default batch size is 200, lower this to 5 to make sure we get multiple batches
	subChangesRequest.SetCompressed(false)

	body := subChangesBody{DocIDs: docIDsExpected}
	bodyBytes, err := json.Marshal(body)
	assertNoError(t, err, "Error marshalling subChanges body.")

	subChangesRequest.SetBody(bodyBytes)

	sent := bt.sender.Send(subChangesRequest)
	assert.True(t, sent)
	subChangesResponse := subChangesRequest.Response()
	assert.Equals(t, subChangesResponse.SerialNumber(), subChangesRequest.SerialNumber())

	// Wait until all expected changes are received by change handler
	// receivedChangesWg.Wait()
	timeoutErr := WaitWithTimeout(&receivedChangesWg, time.Second*15)
	assertNoError(t, timeoutErr, "Timed out waiting for all changes.")

	// Since batch size was set to 10, and 15 docs were added, expect at _least_ 2 batches
	numBatchesReceivedSnapshot := atomic.LoadInt32(&numbatchesReceived)
	assert.True(t, numBatchesReceivedSnapshot >= 2)

	// Validate all expected documents were received.
	for docID, received := range docIDsReceived {
		if !received {
			t.Errorf("Did not receive expected doc %s in changes", docID)
		}
	}

	// Validate that the 'caught up' message was sent
	assert.True(t, receivedCaughtUpChange)
}

// Push proposed changes and ensure that the server accepts them
//
// 1. Start sync gateway in no-conflicts mode
// 2. Send changes push request with multiple doc revisions
// 3. Make sure there are no panics
// 4. Make sure that the server responds to accept the changes (empty array)
func TestProposedChangesNoConflictsMode(t *testing.T) {

	defer base.SetUpTestLogging(base.LevelInfo, base.KeyHTTP|base.KeySync|base.KeySyncMsg)()

	bt, err := NewBlipTesterFromSpec(BlipTesterSpec{
		noConflictsMode: true,
	})
	assertNoError(t, err, "Error creating BlipTester")
	defer bt.Close()

	proposeChangesRequest := blip.NewRequest()
	proposeChangesRequest.SetProfile("proposeChanges")
	proposeChangesRequest.SetCompressed(true)

	// According to proposeChanges spec:
	// proposedChanges entries are of the form: [docID, revID, serverRevID]
	// where serverRevID is optional
	changesBody := `
[["foo", "1-abc"],
["foo2", "1-abc"]]
`
	proposeChangesRequest.SetBody([]byte(changesBody))
	sent := bt.sender.Send(proposeChangesRequest)
	assert.True(t, sent)
	proposeChangesResponse := proposeChangesRequest.Response()
	body, err := proposeChangesResponse.Body()
	assertNoError(t, err, "Error getting changes response body")

	var changeList [][]interface{}
	err = json.Unmarshal(body, &changeList)
	assertNoError(t, err, "Error getting changes response body")

	// The common case of an empty array response tells the sender to send all of the proposed revisions,
	// so the changeList returned by Sync Gateway is expected to be empty
	assert.Equals(t, len(changeList), 0)

}

// Connect to public port with authentication
func TestPublicPortAuthentication(t *testing.T) {

	defer base.SetUpTestLogging(base.LevelInfo, base.KeyHTTP|base.KeySync|base.KeySyncMsg)()

	// Create bliptester that is connected as user1, with access to the user1 channel
	btUser1, err := NewBlipTesterFromSpec(BlipTesterSpec{
		noAdminParty:       true,
		connectingUsername: "user1",
		connectingPassword: "1234",
	})
	assertNoError(t, err, "Error creating BlipTester")
	defer btUser1.Close()

	// Send the user1 doc
	btUser1.SendRev(
		"foo",
		"1-abc",
		[]byte(`{"key": "val", "channels": ["user1"]}`),
		blip.Properties{},
	)

	// Create bliptester that is connected as user2, with access to the * channel
	btUser2, err := NewBlipTesterFromSpec(BlipTesterSpec{
		noAdminParty:                true,
		connectingUsername:          "user2",
		connectingPassword:          "1234",
		connectingUserChannelGrants: []string{"*"},      // user2 has access to all channels
		restTester:                  btUser1.restTester, // re-use rest tester, otherwise it will create a new underlying bucket in walrus case
	})
	assertNoError(t, err, "Error creating BlipTester")
	defer btUser2.Close()

	// Send the user2 doc, which is in a "random" channel, but it should be accessible due to * channel access
	btUser2.SendRev(
		"foo2",
		"1-abcd",
		[]byte(`{"key": "val", "channels": ["NBC"]}`),
		blip.Properties{},
	)

	// Assert that user1 received a single expected change
	changesChannelUser1 := btUser1.WaitForNumChanges(1)
	assert.Equals(t, len(changesChannelUser1), 1)
	change := changesChannelUser1[0]
	AssertChangeEquals(t, change, ExpectedChange{docId: "foo", revId: "1-abc", sequence: "*", deleted: base.BoolPtr(false)})

	// Assert that user2 received user1's change as well as it's own change
	changesChannelUser2 := btUser2.WaitForNumChanges(2)
	assert.Equals(t, len(changesChannelUser2), 2)
	change = changesChannelUser2[0]
	AssertChangeEquals(t, change, ExpectedChange{docId: "foo", revId: "1-abc", sequence: "*", deleted: base.BoolPtr(false)})

	change = changesChannelUser2[1]
	AssertChangeEquals(t, change, ExpectedChange{docId: "foo2", revId: "1-abcd", sequence: "*", deleted: base.BoolPtr(false)})

}

// Test send and retrieval of a doc.
//   Validate deleted handling (includes check for https://github.com/couchbase/sync_gateway/issues/3341)
func TestBlipSendAndGetRev(t *testing.T) {

	defer base.SetUpTestLogging(base.LevelInfo, base.KeyHTTP|base.KeySync|base.KeySyncMsg)()

	// Setup
	rt := RestTester{
		noAdminParty: true,
	}
	btSpec := BlipTesterSpec{
		connectingUsername: "user1",
		connectingPassword: "1234",
		restTester:         &rt,
	}
	bt, err := NewBlipTesterFromSpec(btSpec)
	assertNoError(t, err, "Unexpected error creating BlipTester")
	defer bt.Close()

	// Send non-deleted rev
	sent, _, resp, err := bt.SendRev("sendAndGetRev", "1-abc", []byte(`{"key": "val", "channels": ["user1"]}`), blip.Properties{})
	assert.True(t, sent)
	assert.Equals(t, err, nil)
	assert.Equals(t, resp.Properties["Error-Code"], "")

	// Get non-deleted rev
	response := bt.restTester.SendAdminRequest("GET", "/db/sendAndGetRev?rev=1-abc", "")
	assertStatus(t, response, 200)
	var responseBody RestDocument
	assertNoError(t, json.Unmarshal(response.Body.Bytes(), &responseBody), "Error unmarshalling GET doc response")
	_, ok := responseBody["_deleted"]
	assert.False(t, ok)

	// Tombstone the document
	history := []string{"1-abc"}
	sent, _, resp, err = bt.SendRevWithHistory("sendAndGetRev", "2-bcd", history, []byte(`{"key": "val", "channels": ["user1"]}`), blip.Properties{"deleted": "true"})
	assert.True(t, sent)
	assert.Equals(t, err, nil)
	assert.Equals(t, resp.Properties["Error-Code"], "")

	// Get the tombstoned document
	response = bt.restTester.SendAdminRequest("GET", "/db/sendAndGetRev?rev=2-bcd", "")
	assertStatus(t, response, 200)
	responseBody = RestDocument{}
	assertNoError(t, json.Unmarshal(response.Body.Bytes(), &responseBody), "Error unmarshalling GET doc response")
	deletedValue, deletedOK := responseBody["_deleted"].(bool)
	assert.True(t, deletedOK)
	assert.True(t, deletedValue)
}

func AssertChangeEquals(t *testing.T, change []interface{}, expectedChange ExpectedChange) {
	if err := expectedChange.Equals(change); err != nil {
		t.Errorf("Change %+v does not equal expected change: %+v.  Error: %v", change, expectedChange, err)
	}
}

// Test adding / retrieving attachments
func TestAttachments(t *testing.T) {

}

// Make sure it's not possible to have two outstanding subChanges w/ continuous=true.
// Expected behavior is that the first continous change subscription should get discarded in favor of 2nd.
func TestConcurrentChangesSubscriptions(t *testing.T) {

}

// Create a continous changes subscription that has docs in multiple channels, and make sure
// all docs are received
func TestMultiChannelContinousChangesSubscription(t *testing.T) {

}

// Test setting and getting checkpoints
func TestCheckpoints(t *testing.T) {

}

// Test no-conflicts mode replication (proposeChanges endpoint)
func TestNoConflictsModeReplication(t *testing.T) {

}

// Reproduce issue where ReloadUser was not being called, and so it was
// using a stale channel access grant for the user.
// Reproduces https://github.com/couchbase/sync_gateway/issues/2717
func TestReloadUser(t *testing.T) {

	defer base.SetUpTestLogging(base.LevelInfo, base.KeyHTTP|base.KeySync|base.KeySyncMsg)()

	syncFn := `
		function(doc) {
			if (doc._id == "access1") {
				// if its an access grant doc, grant access
				access(doc.accessUser, doc.accessChannel);
			} else {
                // otherwise if its a normal access doc, require access then add to channels
				requireAccess("PBS");
				channel(doc.channels);
			}
		}
    `

	// Setup
	rt := RestTester{
		SyncFn:       syncFn,
		noAdminParty: true,
	}
	bt, err := NewBlipTesterFromSpec(BlipTesterSpec{
		connectingUsername: "user1",
		connectingPassword: "1234",
		restTester:         &rt,
	})
	assertNoError(t, err, "Unexpected error creating BlipTester")
	defer bt.Close()

	// Put document that triggers access grant for user to channel PBS
	response := rt.SendAdminRequest("PUT", "/db/access1", `{"accessUser":"user1", "accessChannel":["PBS"]}`)
	assertStatus(t, response, 201)

	// Add a doc in the PBS channel
	_, _, addRevResponse, err := bt.SendRev(
		"foo",
		"1-abc",
		[]byte(`{"key": "val", "channels": ["PBS"]}`),
		blip.Properties{},
	)
	assert.Equals(t, err, nil)

	// Make assertions on response to make sure the change was accepted
	addRevResponseBody, err := addRevResponse.Body()
	assertNoError(t, err, "Unexpected error")
	errorCode, hasErrorCode := addRevResponse.Properties["Error-Code"]
	assert.False(t, hasErrorCode)
	if hasErrorCode {
		t.Fatalf("Unexpected error sending revision.  Error code: %v.  Response body: %s", errorCode, addRevResponseBody)
	}

}

// Grant a user access to a channel via the Sync Function and a doc change, and make sure
// it shows up in the user's changes feed
func TestAccessGrantViaSyncFunction(t *testing.T) {

	defer base.SetUpTestLogging(base.LevelInfo, base.KeyHTTP|base.KeySync|base.KeySyncMsg)()

	// Setup
	rt := RestTester{
		SyncFn:       `function(doc) {channel(doc.channels); access(doc.accessUser, doc.accessChannel);}`,
		noAdminParty: true,
	}
	bt, err := NewBlipTesterFromSpec(BlipTesterSpec{
		connectingUsername: "user1",
		connectingPassword: "1234",
		restTester:         &rt,
	})
	assertNoError(t, err, "Unexpected error creating BlipTester")
	defer bt.Close()

	// Add a doc in the PBS channel
	bt.SendRev(
		"foo",
		"1-abc",
		[]byte(`{"key": "val", "channels": ["PBS"]}`),
		blip.Properties{},
	)

	// Put document that triggers access grant for user to channel PBS
	response := rt.SendAdminRequest("PUT", "/db/access1", `{"accessUser":"user1", "accessChannel":["PBS"]}`)
	assertStatus(t, response, 201)

	// Add another doc in the PBS channel
	bt.SendRev(
		"foo2",
		"1-abc",
		[]byte(`{"key": "val", "channels": ["PBS"]}`),
		blip.Properties{},
	)

	// Make sure we can see it by getting changes
	changes := bt.WaitForNumChanges(2)
	log.Printf("changes: %+v", changes)
	assert.Equals(t, len(changes), 2)

}

// Grant a user access to a channel via the REST Admin API, and make sure
// it shows up in the user's changes feed
func TestAccessGrantViaAdminApi(t *testing.T) {

	defer base.SetUpTestLogging(base.LevelInfo, base.KeyHTTP|base.KeySync|base.KeySyncMsg)()

	// Create blip tester
	bt, err := NewBlipTesterFromSpec(BlipTesterSpec{
		noAdminParty:       true,
		connectingUsername: "user1",
		connectingPassword: "1234",
	})
	assertNoError(t, err, "Unexpected error creating BlipTester")
	defer bt.Close()

	// Add a doc in the PBS channel
	bt.SendRev(
		"foo",
		"1-abc",
		[]byte(`{"key": "val", "channels": ["PBS"]}`),
		blip.Properties{},
	)

	// Update the user doc to grant access to PBS
	response := bt.restTester.SendAdminRequest("PUT", "/db/_user/user1", `{"admin_channels":["user1", "PBS"]}`)
	assertStatus(t, response, 200)

	// Add another doc in the PBS channel
	bt.SendRev(
		"foo2",
		"1-abc",
		[]byte(`{"key": "val", "channels": ["PBS"]}`),
		blip.Properties{},
	)

	// Make sure we can see both docs in the changes
	changes := bt.WaitForNumChanges(2)
	assert.Equals(t, len(changes), 2)

}

func TestCheckpoint(t *testing.T) {

	defer base.SetUpTestLogging(base.LevelInfo, base.KeyHTTP|base.KeySync|base.KeySyncMsg)()

	// Create blip tester
	bt, err := NewBlipTesterFromSpec(BlipTesterSpec{
		noAdminParty:       true,
		connectingUsername: "user1",
		connectingPassword: "1234",
	})
	assertNoError(t, err, "Unexpected error creating BlipTester")
	defer bt.Close()

	client := "testClient"

	// Get the checkpoint -- expect to be missing at this point
	request := blip.NewRequest()
	request.SetCompressed(true)
	request.SetProfile("getCheckpoint")
	request.Properties["client"] = client
	sent := bt.sender.Send(request)
	if !sent {
		panic(fmt.Sprintf("Failed to get checkpoint for client: %v", client))
	}
	checkpointResponse := request.Response()

	// Expect to get no checkpoint
	errorcode, ok := checkpointResponse.Properties["Error-Code"]
	assert.True(t, ok)
	assert.Equals(t, errorcode, "404")

	// Set a checkpoint
	requestSetCheckpoint := blip.NewRequest()
	requestSetCheckpoint.SetCompressed(true)
	requestSetCheckpoint.SetProfile("setCheckpoint")
	requestSetCheckpoint.Properties["client"] = client
	checkpointBody := db.Body{"Key": "Value"}
	requestSetCheckpoint.SetJSONBody(checkpointBody)
	// requestSetCheckpoint.Properties["rev"] = "rev1"
	sent = bt.sender.Send(requestSetCheckpoint)
	if !sent {
		panic(fmt.Sprintf("Failed to set checkpoint for client: %v", client))
	}
	checkpointResponse = requestSetCheckpoint.Response()
	body, err := checkpointResponse.Body()
	assertNoError(t, err, "Unexpected error")
	log.Printf("responseSetCheckpoint body: %s", body)

	// Get the checkpoint and make sure it has the expected value
	requestGetCheckpoint2 := blip.NewRequest()
	requestGetCheckpoint2.SetCompressed(true)
	requestGetCheckpoint2.SetProfile("getCheckpoint")
	requestGetCheckpoint2.Properties["client"] = client
	sent = bt.sender.Send(requestGetCheckpoint2)
	if !sent {
		panic(fmt.Sprintf("Failed to get checkpoint for client: %v", client))
	}
	checkpointResponse = requestGetCheckpoint2.Response()
	body, err = checkpointResponse.Body()
	assertNoError(t, err, "Unexpected error")
	log.Printf("body: %s", body)
	assert.True(t, strings.Contains(string(body), "Key"))
	assert.True(t, strings.Contains(string(body), "Value"))

}

// Test Attachment replication behavior described here: https://github.com/couchbase/couchbase-lite-core/wiki/Replication-Protocol
// - Put attachment via blip
// - Verifies that getAttachment won't return attachment "out of context" of a rev request
// - Get attachment via REST and verifies it returns the correct content
func TestPutAttachmentViaBlipGetViaRest(t *testing.T) {

	defer base.SetUpTestLogging(base.LevelInfo, base.KeyHTTP|base.KeySync|base.KeySyncMsg)()

	// Create blip tester
	bt, err := NewBlipTesterFromSpec(BlipTesterSpec{
		noAdminParty:       true,
		connectingUsername: "user1",
		connectingPassword: "1234",
	})
	assertNoError(t, err, "Unexpected error creating BlipTester")
	defer bt.Close()

	input := SendRevWithAttachmentInput{
		docId:            "doc",
		revId:            "1-rev1",
		attachmentName:   "myAttachment",
		attachmentBody:   "attach",
		attachmentDigest: "fakedigest",
	}
	bt.SendRevWithAttachment(input)

	// Try to fetch the attachment directly via getAttachment, expected to fail w/ 403 error for security reasons
	// since it's not in the context of responding to a "rev" request from the peer.
	getAttachmentRequest := blip.NewRequest()
	getAttachmentRequest.SetProfile("getAttachment")
	getAttachmentRequest.Properties["digest"] = input.attachmentDigest
	sent := bt.sender.Send(getAttachmentRequest)
	if !sent {
		panic(fmt.Sprintf("Failed to send request for doc: %v", input.docId))
	}
	getAttachmentResponse := getAttachmentRequest.Response()
	errorCode, hasErrorCode := getAttachmentResponse.Properties["Error-Code"]
	assert.Equals(t, errorCode, "403") // "Attachment's doc not being synced"
	assert.True(t, hasErrorCode)

	// Get the attachment via REST api and make sure it matches the attachment pushed earlier
	response := bt.restTester.SendAdminRequest("GET", fmt.Sprintf("/db/%s/%s", input.docId, input.attachmentName), ``)
	assert.Equals(t, response.Body.String(), input.attachmentBody)

}

func TestPutAttachmentViaBlipGetViaBlip(t *testing.T) {

	defer base.SetUpTestLogging(base.LevelInfo, base.KeyHTTP|base.KeySync|base.KeySyncMsg)()

	// Create blip tester
	bt, err := NewBlipTesterFromSpec(BlipTesterSpec{
		noAdminParty:                true,
		connectingUsername:          "user1",
		connectingPassword:          "1234",
		connectingUserChannelGrants: []string{"*"}, // All channels
	})
	assertNoError(t, err, "Unexpected error creating BlipTester")
	defer bt.Close()

	attachmentBody := "attach"

	digest := db.Sha1DigestKey([]byte(attachmentBody))

	// Send revision with attachment
	input := SendRevWithAttachmentInput{
		docId:            "doc",
		revId:            "1-rev1",
		attachmentName:   "myAttachment",
		attachmentBody:   attachmentBody,
		attachmentDigest: digest,
	}
	sent, _, _ := bt.SendRevWithAttachment(input)
	assert.True(t, sent)

	// Get all docs and attachment via subChanges request
	allDocs := bt.WaitForNumDocsViaChanges(1)

	// make assertions on allDocs -- make sure attachment is present w/ expected body
	assert.Equals(t, len(allDocs), 1)
	retrievedDoc := allDocs[input.docId]

	// doc assertions
	assert.Equals(t, retrievedDoc.ID(), input.docId)
	assert.Equals(t, retrievedDoc.RevID(), input.revId)

	// attachment assertions
	attachments, err := retrievedDoc.GetAttachments()
	assert.True(t, err == nil)
	assert.Equals(t, len(attachments), 1)
	retrievedAttachment := attachments[input.attachmentName]
	assert.Equals(t, string(retrievedAttachment.Data), input.attachmentBody)
	assert.Equals(t, retrievedAttachment.Length, len(attachmentBody))
	assert.Equals(t, input.attachmentDigest, retrievedAttachment.Digest)

}

// Put a revision that is rejected by the sync function and assert that Sync Gateway
// returns an error code
func TestPutInvalidRevSyncFnReject(t *testing.T) {

	defer base.SetUpTestLogging(base.LevelInfo, base.KeyHTTP|base.KeySync|base.KeySyncMsg)()

	syncFn := `
		function(doc) {
			requireAccess("PBS");
			channel(doc.channels);
		}
    `

	// Setup
	rt := RestTester{
		SyncFn:       syncFn,
		noAdminParty: true,
	}
	bt, err := NewBlipTesterFromSpec(BlipTesterSpec{
		connectingUsername: "user1",
		connectingPassword: "1234",
		restTester:         &rt,
	})
	assertNoError(t, err, "Unexpected error creating BlipTester")
	defer bt.Close()

	// Add a doc that will be rejected by sync function, since user
	// does not have access to the CNN channel
	revRequest := blip.NewRequest()
	revRequest.SetCompressed(false)
	revRequest.SetProfile("rev")
	revRequest.Properties["id"] = "foo"
	revRequest.Properties["rev"] = "1-aaa"
	revRequest.Properties["deleted"] = "false"
	revRequest.SetBody([]byte(`{"key": "val", "channels": ["CNN"]}`))
	sent := bt.sender.Send(revRequest)
	assert.True(t, sent)

	revResponse := revRequest.Response()

	// Since doc is rejected by sync function, expect a 403 error
	errorCode, hasErrorCode := revResponse.Properties["Error-Code"]
	assert.True(t, hasErrorCode)
	assert.Equals(t, errorCode, "403")

	// Make sure that a one-off GetChanges() returns no documents
	changes := bt.GetChanges()
	assert.Equals(t, len(changes), 0)

}

func TestPutInvalidRevMalformedBody(t *testing.T) {

	defer base.SetUpTestLogging(base.LevelInfo, base.KeyHTTP|base.KeySync|base.KeySyncMsg)()

	// Create blip tester
	bt, err := NewBlipTesterFromSpec(BlipTesterSpec{
		noAdminParty:                true,
		connectingUsername:          "user1",
		connectingPassword:          "1234",
		connectingUserChannelGrants: []string{"*"}, // All channels
	})
	assertNoError(t, err, "Unexpected error creating BlipTester")
	defer bt.Close()

	// Add a doc that will be rejected by sync function, since user
	// does not have access to the CNN channel
	revRequest := blip.NewRequest()
	revRequest.SetCompressed(false)
	revRequest.SetProfile("rev")
	revRequest.Properties["deleted"] = "false"
	revRequest.SetBody([]byte(`{"key": "val", "channels": [" MALFORMED JSON DOC`))

	sent := bt.sender.Send(revRequest)
	assert.True(t, sent)

	revResponse := revRequest.Response()

	// Since doc is rejected by sync function, expect a 403 error
	errorCode, hasErrorCode := revResponse.Properties["Error-Code"]
	assert.True(t, hasErrorCode)
	assert.Equals(t, errorCode, "500")

	// Make sure that a one-off GetChanges() returns no documents
	changes := bt.GetChanges()
	assert.Equals(t, len(changes), 0)

}

func TestPutRevNoConflictsMode(t *testing.T) {

	defer base.SetUpTestLogging(base.LevelInfo, base.KeyHTTP|base.KeySync|base.KeySyncMsg)()

	// Create blip tester
	bt, err := NewBlipTesterFromSpec(BlipTesterSpec{
		noConflictsMode: true,
	})
	assertNoError(t, err, "Unexpected error creating BlipTester")
	defer bt.Close()

	sent, _, resp, err := bt.SendRev("foo", "1-abc", []byte(`{"key": "val"}`), blip.Properties{})
	assert.True(t, sent)
	assert.Equals(t, err, nil)                          // no error
	assert.Equals(t, resp.Properties["Error-Code"], "") // no error

	sent, _, resp, err = bt.SendRev("foo", "1-def", []byte(`{"key": "val"}`), blip.Properties{"noconflicts": "true"})
	assert.True(t, sent)
	assert.NotEquals(t, err, nil)                          // conflict error
	assert.Equals(t, resp.Properties["Error-Code"], "409") // conflict

	sent, _, resp, err = bt.SendRev("foo", "1-ghi", []byte(`{"key": "val"}`), blip.Properties{"noconflicts": "false"})
	assert.True(t, sent)
	assert.NotEquals(t, err, nil)                          // conflict error
	assert.Equals(t, resp.Properties["Error-Code"], "409") // conflict

}

func TestPutRevConflictsMode(t *testing.T) {

	defer base.SetUpTestLogging(base.LevelInfo, base.KeyHTTP|base.KeySync|base.KeySyncMsg)()

	// Create blip tester
	bt, err := NewBlipTesterFromSpec(BlipTesterSpec{
		noConflictsMode: false,
	})
	assertNoError(t, err, "Unexpected error creating BlipTester")
	defer bt.Close()

	sent, _, resp, err := bt.SendRev("foo", "1-abc", []byte(`{"key": "val"}`), blip.Properties{})
	assert.True(t, sent)
	assert.Equals(t, err, nil)                          // no error
	assert.Equals(t, resp.Properties["Error-Code"], "") // no error

	sent, _, resp, err = bt.SendRev("foo", "1-def", []byte(`{"key": "val"}`), blip.Properties{"noconflicts": "false"})
	assert.True(t, sent)
	assert.Equals(t, err, nil)                          // no error
	assert.Equals(t, resp.Properties["Error-Code"], "") // no error

	sent, _, resp, err = bt.SendRev("foo", "1-ghi", []byte(`{"key": "val"}`), blip.Properties{"noconflicts": "true"})
	assert.True(t, sent)
	assert.NotEquals(t, err, nil)                          // conflict error
	assert.Equals(t, resp.Properties["Error-Code"], "409") // conflict

}

// Repro attempt for SG #3281
//
// - Set up a user w/ access to channel A
// - Write two revision of a document (both in channel A)
// - Write two more revisions of the document, no longer in channel A
// - Have the user issue a rev request for rev 3.
//
// Expected:
// - Users gets a removed:true response
//
// Actual:
// - Same as Expected (this test is unable to repro SG #3281, but is being left in as a regression test)
//
func TestGetRemovedDoc(t *testing.T) {

	defer base.SetUpTestLogging(base.LevelInfo, base.KeyHTTP|base.KeySync|base.KeySyncMsg)()

	// Setup
	rt := RestTester{
		noAdminParty: true,
	}
	btSpec := BlipTesterSpec{
		connectingUsername: "user1",
		connectingPassword: "1234",
		restTester:         &rt,
	}
	bt, err := NewBlipTesterFromSpec(btSpec)
	assertNoError(t, err, "Unexpected error creating BlipTester")
	defer bt.Close()

	// Add rev-1 in channel user1
	sent, _, resp, err := bt.SendRev("foo", "1-abc", []byte(`{"key": "val", "channels": ["user1"]}"`), blip.Properties{})
	assert.True(t, sent)
	assert.Equals(t, err, nil)                          // no error
	assert.Equals(t, resp.Properties["Error-Code"], "") // no error

	// Add rev-2 in channel user1
	history := []string{"1-abc"}
	sent, _, resp, err = bt.SendRevWithHistory("foo", "2-bcd", history, []byte(`{"key": "val", "channels": ["user1"]}"`), blip.Properties{"noconflicts": "true"})
	assert.True(t, sent)
	assert.Equals(t, err, nil)                          // no error
	assert.Equals(t, resp.Properties["Error-Code"], "") // no error

	// Try to get rev 2 via BLIP API and assert that _removed == false
	resultDoc, err := bt.GetDocAtRev("foo", "2-bcd")
	assertNoError(t, err, "Unexpected Error")
	assert.False(t, resultDoc.IsRemoved())

	// Add rev-3, remove from channel user1 and put into channel another_channel
	history = []string{"2-bcd", "1-abc"}
	sent, _, resp, err = bt.SendRevWithHistory("foo", "3-cde", history, []byte(`{"key": "val", "channels": ["another_channel"]}`), blip.Properties{"noconflicts": "true"})
	assert.True(t, sent)
	assert.Equals(t, err, nil)                          // no error
	assert.Equals(t, resp.Properties["Error-Code"], "") // no error

	// Add rev-4, keeping it in channel another_channel
	history = []string{"3-cde", "2-bcd", "1-abc"}
	sent, _, resp, err = bt.SendRevWithHistory("foo", "4-def", history, []byte("{}"), blip.Properties{"noconflicts": "true", "deleted": "true"})
	assert.True(t, sent)
	assert.Equals(t, err, nil)                          // no error
	assert.Equals(t, resp.Properties["Error-Code"], "") // no error

	// Flush rev cache in case this prevents the bug from showing up (didn't make a difference)
	rt.GetDatabase().FlushRevisionCache()

	// Delete any temp revisions in case this prevents the bug from showing up (didn't make a difference)
	tempRevisionDocId := "_sync:rev:foo:5:3-cde"
	err = rt.GetDatabase().Bucket.Delete(tempRevisionDocId)
	assertNoError(t, err, "Unexpected Error")

	// Workaround data race (https://gist.github.com/tleyden/0ace70b8a38b76a7beee95529610b6cf) that happens because
	// there are multiple goroutines accessing the bt.blipContext.HandlerForProfile map.
	// The workaround uses a separate blipTester, and therefore a separate context.  It uses a different
	// user to avoid an error when the NewBlipTesterFromSpec tries to create the user (eg, user1 already exists error)
	btSpec2 := BlipTesterSpec{
		connectingUsername:          "user2",
		connectingPassword:          "1234",
		connectingUserChannelGrants: []string{"user1"}, // so it can see user1's docs
		restTester:                  &rt,
	}
	bt2, err := NewBlipTesterFromSpec(btSpec2)
	assertNoError(t, err, "Unexpected error creating BlipTester")

	// Try to get rev 3 via BLIP API and assert that _removed == true
	resultDoc, err = bt2.GetDocAtRev("foo", "3-cde")
	assertNoError(t, err, "Unexpected Error")
	assert.True(t, resultDoc.IsRemoved())

	// Try to get rev 3 via REST API, and assert that _removed == true
	headers := map[string]string{}
	headers["Authorization"] = "Basic " + base64.StdEncoding.EncodeToString([]byte(btSpec.connectingUsername+":"+btSpec.connectingPassword))
	response := rt.SendRequestWithHeaders("GET", "/db/foo?rev=3-cde", "", headers)
	restDocument := response.GetRestDocument()
	assert.True(t, restDocument.IsRemoved())

}

// Make sure that a client cannot open multiple subChanges subscriptions on a single blip context (SG #3222)
//
// - Open two continuous subChanges feeds, and asserts that it gets an error on the 2nd one.
// - Open a one-off subChanges request, assert no error
func TestMultipleOustandingChangesSubscriptions(t *testing.T) {

	defer base.SetUpTestLogging(base.LevelInfo, base.KeyHTTP|base.KeySync|base.KeySyncMsg)()

	bt, err := NewBlipTester()
	assertNoError(t, err, "Error creating BlipTester")
	defer bt.Close()

	bt.blipContext.HandlerForProfile["changes"] = func(request *blip.Message) {
		if !request.NoReply() {
			// Send an empty response to avoid the Sync: Invalid response to 'changes' message
			response := request.Response()
			emptyResponseVal := []interface{}{}
			emptyResponseValBytes, err := json.Marshal(emptyResponseVal)
			assertNoError(t, err, "Error marshalling response")
			response.SetBody(emptyResponseValBytes)
		}
	}

	// Send continous subChanges to subscribe to changes, which will cause the "changes" profile handler above to be called back
	subChangesRequest := blip.NewRequest()
	subChangesRequest.SetProfile("subChanges")
	subChangesRequest.Properties["continuous"] = "true"
	subChangesRequest.SetCompressed(false)
	sent := bt.sender.Send(subChangesRequest)
	assert.True(t, sent)
	subChangesResponse := subChangesRequest.Response()
	assert.Equals(t, subChangesResponse.SerialNumber(), subChangesRequest.SerialNumber())
	errorCode := subChangesResponse.Properties["Error-Code"]
	log.Printf("errorCode: %v", errorCode)
	assert.True(t, errorCode == "")

	// Send a second continuous subchanges request, expect an error
	subChangesRequest2 := blip.NewRequest()
	subChangesRequest2.SetProfile("subChanges")
	subChangesRequest2.Properties["continuous"] = "true"
	subChangesRequest2.SetCompressed(false)
	sent2 := bt.sender.Send(subChangesRequest2)
	assert.True(t, sent2)
	subChangesResponse2 := subChangesRequest2.Response()
	assert.Equals(t, subChangesResponse2.SerialNumber(), subChangesRequest2.SerialNumber())
	errorCode2 := subChangesResponse2.Properties["Error-Code"]
	log.Printf("errorCode2: %v", errorCode2)
	assert.True(t, errorCode2 == "500")

	// Send a thirst subChanges request, but this time continuous = false.  Should not return an error
	subChangesRequest3 := blip.NewRequest()
	subChangesRequest3.SetProfile("subChanges")
	subChangesRequest3.Properties["continuous"] = "false"
	subChangesRequest3.SetCompressed(false)
	sent3 := bt.sender.Send(subChangesRequest3)
	assert.True(t, sent3)
	subChangesResponse3 := subChangesRequest3.Response()
	assert.Equals(t, subChangesResponse3.SerialNumber(), subChangesRequest3.SerialNumber())
	errorCode3 := subChangesResponse3.Properties["Error-Code"]
	log.Printf("errorCode: %v", errorCode3)
	assert.True(t, errorCode == "")

}
