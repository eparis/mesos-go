package sasl

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"code.google.com/p/gogoprotobuf/proto"
	log "github.com/golang/glog"
	"github.com/mesos/mesos-go/auth"
	"github.com/mesos/mesos-go/auth/callback"
	"github.com/mesos/mesos-go/auth/sasl/mech"
	mesos "github.com/mesos/mesos-go/mesosproto"
	"github.com/mesos/mesos-go/messenger"
	"github.com/mesos/mesos-go/upid"
	"golang.org/x/net/context"
)

var (
	pidLock sync.Mutex
	pid     int

	UnexpectedAuthenticationMechanisms = errors.New("Unexpected authentication 'mechanisms' received")
	UnexpectedAuthenticationStep       = errors.New("Unexpected authentication 'step' received")
	UnexpectedAuthenticationCompleted  = errors.New("Unexpected authentication 'completed' received")
	UnexpectedAuthenticatorPid         = errors.New("Unexpected authentator pid") // authenticator pid changed mid-process
	UnsupportedMechanism               = errors.New("failed to identify a compatible mechanism")
)

type statusType int32

const (
	statusReady statusType = iota
	statusStarting
	statusStepping
	_statusTerminal // meta status, should never be assigned: all status types following are "terminal"
	statusCompleted
	statusFailed
	statusError
	statusDiscarded

	// this login provider name is automatically registered with the auth package; see init()
	ProviderName = "SASL"
)

type authenticateeProcess struct {
	transport messenger.Messenger
	client    upid.UPID
	status    statusType
	done      chan struct{}
	err       error
	mech      mech.Interface
	stepFn    mech.StepFunc
	from      *upid.UPID
	handler   callback.Handler
}

type authenticateeConfig struct {
	client    upid.UPID // pid of the client we're attempting to authenticate
	handler   callback.Handler
	transport messenger.Messenger // mesos communications transport
}

func init() {
	if err := auth.RegisterAuthenticateeProvider(ProviderName, auth.AuthenticateeFunc(Authenticatee)); err != nil {
		log.Error(err)
	}
}

func nextPid() int {
	pidLock.Lock()
	defer pidLock.Unlock()
	pid++
	return pid
}

func (s *statusType) get() statusType {
	return statusType(atomic.LoadInt32((*int32)(s)))
}

func (s *statusType) swap(old, new statusType) bool {
	return old != new && atomic.CompareAndSwapInt32((*int32)(s), int32(old), int32(new))
}

func Authenticatee(ctx context.Context, handler callback.Handler) <-chan error {

	ip := callback.NewInterprocess()
	if err := handler.Handle(ip); err != nil {
		return auth.LoginError(err)
	}

	c := make(chan error, 1)
	f := func() error {
		config := newConfig(ip.Client(), handler)
		ctx, auth := newAuthenticatee(ctx, config)
		auth.authenticate(ctx, ip.Server())

		select {
		case <-ctx.Done():
			return auth.discard(ctx)
		case <-auth.done:
			return auth.err
		}
	}
	go func() { c <- f() }()
	return c
}

func newConfig(client upid.UPID, handler callback.Handler) *authenticateeConfig {
	tpid := &upid.UPID{ID: fmt.Sprintf("sasl_authenticatee(%d)", nextPid())}
	return &authenticateeConfig{
		client:    client,
		handler:   handler,
		transport: messenger.NewMesosMessenger(tpid),
	}
}

// Terminate the authentication process upon context cancellation;
// only to be called if/when ctx.Done() has been signalled.
func (self *authenticateeProcess) discard(ctx context.Context) error {
	err := ctx.Err()
	status := statusFrom(ctx)
	for ; status < _statusTerminal; status = (&self.status).get() {
		if self.terminate(status, statusDiscarded, err) {
			break
		}
	}
	return err
}

func newAuthenticatee(ctx context.Context, config *authenticateeConfig) (context.Context, *authenticateeProcess) {
	initialStatus := statusReady
	proc := &authenticateeProcess{
		transport: config.transport,
		client:    config.client,
		handler:   config.handler,
		status:    initialStatus,
		done:      make(chan struct{}),
	}
	ctx = withStatus(ctx, initialStatus)
	err := proc.installHandlers(ctx)
	if err == nil {
		err = proc.startTransport()
	}
	if err != nil {
		proc.terminate(initialStatus, statusError, err)
	}
	return ctx, proc
}

func (self *authenticateeProcess) startTransport() error {
	if err := self.transport.Start(); err != nil {
		return err
	} else {
		go func() {
			// stop the authentication transport upon termination of the
			// authenticator process
			select {
			case <-self.done:
				log.V(2).Infof("stopping authenticator transport: %v", self.transport.UPID())
				self.transport.Stop()
			}
		}()
	}
	return nil
}

// returns true when handlers are installed without error, otherwise terminates the
// authentication process.
func (self *authenticateeProcess) installHandlers(ctx context.Context) error {

	type handlerFn func(ctx context.Context, from *upid.UPID, pbMsg proto.Message)

	withContext := func(f handlerFn) messenger.MessageHandler {
		return func(from *upid.UPID, m proto.Message) {
			status := (&self.status).get()
			if self.from != nil && !self.from.Equal(from) {
				self.terminate(status, statusError, UnexpectedAuthenticatorPid)
			} else {
				f(withStatus(ctx, status), from, m)
			}
		}
	}

	// Anticipate mechanisms and steps from the server
	handlers := []struct {
		f handlerFn
		m proto.Message
	}{
		{self.mechanisms, &mesos.AuthenticationMechanismsMessage{}},
		{self.step, &mesos.AuthenticationStepMessage{}},
		{self.completed, &mesos.AuthenticationCompletedMessage{}},
		{self.failed, &mesos.AuthenticationFailedMessage{}},
		{self.errored, &mesos.AuthenticationErrorMessage{}},
	}
	for _, h := range handlers {
		if err := self.transport.Install(withContext(h.f), h.m); err != nil {
			return err
		}
	}
	return nil
}

// return true if the authentication status was updated (if true, self.done will have been closed)
func (self *authenticateeProcess) terminate(old, new statusType, err error) bool {
	if (&self.status).swap(old, new) {
		self.err = err
		if self.mech != nil {
			self.mech.Discard()
		}
		close(self.done)
		return true
	}
	return false
}

func (self *authenticateeProcess) authenticate(ctx context.Context, pid upid.UPID) {
	status := statusFrom(ctx)
	if status != statusReady {
		return
	}
	message := &mesos.AuthenticateMessage{
		Pid: proto.String(self.client.String()),
	}
	if err := self.transport.Send(ctx, &pid, message); err != nil {
		self.terminate(status, statusError, err)
	} else {
		(&self.status).swap(status, statusStarting)
	}
}

func (self *authenticateeProcess) mechanisms(ctx context.Context, from *upid.UPID, pbMsg proto.Message) {
	status := statusFrom(ctx)
	if status != statusStarting {
		self.terminate(status, statusError, UnexpectedAuthenticationMechanisms)
		return
	}

	msg, ok := pbMsg.(*mesos.AuthenticationMechanismsMessage)
	if !ok {
		self.terminate(status, statusError, fmt.Errorf("Expected AuthenticationMechanismsMessage, not %T", pbMsg))
		return
	}

	mechanisms := msg.GetMechanisms()
	log.Infof("Received SASL authentication mechanisms: %v", mechanisms)

	selectedMech, factory := mech.SelectSupported(mechanisms)
	if selectedMech == "" {
		self.terminate(status, statusError, UnsupportedMechanism)
		return
	}

	if m, f, err := factory(self.handler); err != nil {
		self.terminate(status, statusError, err)
		return
	} else {
		self.mech = m
		self.stepFn = f
		self.from = from
	}

	// execute initialization step...
	nextf, data, err := self.stepFn(self.mech, nil)
	if err != nil {
		self.terminate(status, statusError, err)
		return
	} else {
		self.stepFn = nextf
	}

	message := &mesos.AuthenticationStartMessage{
		Mechanism: proto.String(selectedMech),
		Data:      proto.String(string(data)), // may be nil, depends on init step
	}

	if err := self.transport.Send(ctx, from, message); err != nil {
		self.terminate(status, statusError, err)
	} else {
		(&self.status).swap(status, statusStepping)
	}
}

func (self *authenticateeProcess) step(ctx context.Context, from *upid.UPID, pbMsg proto.Message) {
	status := statusFrom(ctx)
	if status != statusStepping {
		self.terminate(status, statusError, UnexpectedAuthenticationStep)
		return
	}

	log.Info("Received SASL authentication step")

	msg, ok := pbMsg.(*mesos.AuthenticationStepMessage)
	if !ok {
		self.terminate(status, statusError, fmt.Errorf("Expected AuthenticationStepMessage, not %T", pbMsg))
		return
	}

	input := msg.GetData()
	fn, output, err := self.stepFn(self.mech, input)

	if err != nil {
		self.terminate(status, statusError, fmt.Errorf("failed to perform authentication step: %v", err))
		return
	}
	self.stepFn = fn

	// We don't start the client with SASL_SUCCESS_DATA so we may
	// need to send one more "empty" message to the server.
	message := &mesos.AuthenticationStepMessage{}
	if len(output) > 0 {
		message.Data = output
	}
	if err := self.transport.Send(ctx, from, message); err != nil {
		self.terminate(status, statusError, err)
	}
}

func (self *authenticateeProcess) completed(ctx context.Context, from *upid.UPID, pbMsg proto.Message) {
	status := statusFrom(ctx)
	if status != statusStepping {
		self.terminate(status, statusError, UnexpectedAuthenticationCompleted)
		return
	}

	log.Info("Authentication success")
	self.terminate(status, statusCompleted, nil)
}

func (self *authenticateeProcess) failed(ctx context.Context, from *upid.UPID, pbMsg proto.Message) {
	status := statusFrom(ctx)
	self.terminate(status, statusFailed, auth.AuthenticationFailed)
}

func (self *authenticateeProcess) errored(ctx context.Context, from *upid.UPID, pbMsg proto.Message) {
	var err error
	if msg, ok := pbMsg.(*mesos.AuthenticationErrorMessage); !ok {
		err = fmt.Errorf("Expected AuthenticationErrorMessage, not %T", pbMsg)
	} else {
		err = fmt.Errorf("Authentication error: %s", msg.GetError())
	}
	status := statusFrom(ctx)
	self.terminate(status, statusError, err)
}
