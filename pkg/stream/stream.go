package stream

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"time"

	"github.com/pkg/errors"
)

// PeerFactory should return the current set of peer addresses.
// Each address will be converted to an io.Reader via the ReaderFactory.
// The PeerFactory is periodically invoked to get the latest set of peers.
type PeerFactory func() []string

// ReaderFactory converts a peer address to an io.Reader.
// Readers must exit with context.Canceled when the context is canceled.
// Other errors will cause the managing goroutine to reconstruct the reader.
type ReaderFactory func(context.Context, string) (io.Reader, error)

// Execute creates and maintains streams of records to multiple peers.
// It blocks until the parent context is canceled.
// It's designed to be invoked once per user stream request.
//
// Incoming records are muxed onto the provided sink chan.
// The sleep func is used to backoff between retries of a single peer.
// The ticker func is used to regularly resolve peers.
func Execute(
	ctx context.Context,
	pf PeerFactory,
	rf ReaderFactory,
	sink chan<- []byte,
	sleep func(time.Duration),
	ticker func(time.Duration) *time.Ticker,
) {
	// Invoke the PeerFactory to get the initial addrs.
	// Initialize connection managers to each of them.
	active := updateActive(ctx, nil, pf(), rf, sink, sleep)

	// Re-invoke the peerFactory every second.
	tk := ticker(time.Second)
	defer tk.Stop()

	for {
		select {
		case <-tk.C:
			// Detect new peers, and create connection managers for them.
			// Terminate connection managers for peers that have gone away.
			active = updateActive(ctx, active, pf(), rf, sink, sleep) // update

		case <-ctx.Done():
			// Context cancelation is transitive.
			// We just need to exit.
			return
		}
	}
}

func updateActive(
	parent context.Context,
	prevgen map[string]func(),
	addrs []string,
	rf ReaderFactory,
	sink chan<- []byte,
	sleep func(time.Duration),
) map[string]func() {
	// Create the "new" collection of peer managers.
	// Really, we just have to track the cancel func.
	nextgen := map[string]func(){}

	// The addrs represent all the connections we *should* have.
	for _, addr := range addrs {
		if cancel, ok := prevgen[addr]; ok {
			// This addr already exists in our previous collection.
			// Just move its cancel func over to the new collection.
			nextgen[addr] = cancel
			delete(prevgen, addr)
		} else {
			// This addr appears to be new!
			// Create a new connection manager for it.
			ctx, cancel := context.WithCancel(parent)
			go readUntilCanceled(ctx, rf, addr, sink, sleep)
			nextgen[addr] = cancel
		}
	}

	// All the addrs left over in the previous collection are gone.
	// Their connection managers should be canceled.
	for _, cancel := range prevgen {
		cancel()
	}

	// Good to go.
	return nextgen
}

// readUntilCanceled is a kind of connection manager to the given addr.
// We connect to addr via the factory, read records, and put them on the sink.
// Any connection error causes us to wait a second and then reconnect.
// readUntilCanceled blocks until the context is canceled.
func readUntilCanceled(ctx context.Context, rf ReaderFactory, addr string, sink chan<- []byte, sleep func(time.Duration)) {
	for {
		switch readOnce(ctx, rf, addr, sink) {
		case context.Canceled:
			return
		default:
			sleep(time.Second)
		}
	}
}

func readOnce(ctx context.Context, rf ReaderFactory, addr string, sink chan<- []byte) error {
	r, err := rf(ctx, addr)
	if err != nil {
		return err
	}
	s := bufio.NewScanner(r)
	for s.Scan() {
		select {
		case sink <- append(s.Bytes(), '\n'):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.Err()
}

// HTTPReaderFactory returns a ReaderFactory that converts the addr to a URL via
// the addr2url function, makes a GET request via the client, and returns the
// response body as the reader.
func HTTPReaderFactory(client *http.Client, addr2url func(string) string) ReaderFactory {
	return func(ctx context.Context, addr string) (io.Reader, error) {
		req, err := http.NewRequest("GET", addr2url(addr), nil)
		if err != nil {
			return nil, errors.Wrap(err, "NewRequest")
		}
		resp, err := client.Do(req.WithContext(ctx))
		if err != nil {
			return nil, errors.Wrap(err, "Do")
		}
		if resp.StatusCode != http.StatusOK {
			return nil, errors.Errorf("GET: %s", resp.Status)
		}
		return resp.Body, nil
	}
}
