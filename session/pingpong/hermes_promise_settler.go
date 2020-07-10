/*
 * Copyright (C) 2019 The "MysteriumNetwork/node" Authors.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package pingpong

import (
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	nodevent "github.com/mysteriumnetwork/node/core/node/event"
	"github.com/mysteriumnetwork/node/core/service/servicestate"
	"github.com/mysteriumnetwork/node/eventbus"
	"github.com/mysteriumnetwork/node/identity"
	"github.com/mysteriumnetwork/node/identity/registry"
	"github.com/mysteriumnetwork/node/session/pingpong/event"
	"github.com/mysteriumnetwork/payments/bindings"
	"github.com/mysteriumnetwork/payments/client"
	"github.com/mysteriumnetwork/payments/crypto"
	"github.com/pkg/errors"
	"github.com/rs/zerolog/log"
)

type settlementHistoryStorage interface {
	Store(provider identity.Identity, hermes common.Address, she SettlementHistoryEntry) error
}

type providerChannelStatusProvider interface {
	SubscribeToPromiseSettledEvent(providerID, hermesID common.Address) (sink chan *bindings.HermesImplementationPromiseSettled, cancel func(), err error)
	GetProviderChannel(hermesAddress common.Address, addressToCheck common.Address, pending bool) (client.ProviderChannel, error)
	GetHermesFee(hermesAddress common.Address) (uint16, error)
}

type ks interface {
	Accounts() []accounts.Account
}

type registrationStatusProvider interface {
	GetRegistrationStatus(id identity.Identity) (registry.RegistrationStatus, error)
}

type transactor interface {
	FetchSettleFees() (registry.FeesResponse, error)
	SettleAndRebalance(hermesID, providerID string, promise crypto.Promise) error
	SettleWithBeneficiary(id, beneficiary, hermesID string, promise crypto.Promise) error
	SettleIntoStake(hermesID, providerID string, promise crypto.Promise) error
}

type promiseStorage interface {
	Get(id identity.Identity, hermesID common.Address) (HermesPromise, error)
}

type receivedPromise struct {
	provider identity.Identity
	hermesID common.Address
	promise  crypto.Promise
}

// HermesPromiseSettler is responsible for settling the hermes promises.
type HermesPromiseSettler interface {
	GetEarnings(id identity.Identity) event.Earnings
	ForceSettle(providerID identity.Identity, hermesID common.Address) error
	SettleWithBeneficiary(providerID identity.Identity, beneficiary, hermesID common.Address) error
	SettleIntoStake(providerID identity.Identity, hermesID common.Address) error
	GetHermesFee(common.Address) (uint16, error)
	Subscribe() error
}

// hermesPromiseSettler is responsible for settling the hermes promises.
type hermesPromiseSettler struct {
	eventBus                   eventbus.EventBus
	bc                         providerChannelStatusProvider
	config                     HermesPromiseSettlerConfig
	lock                       sync.RWMutex
	registrationStatusProvider registrationStatusProvider
	ks                         ks
	transactor                 transactor
	promiseStorage             promiseStorage
	settlementHistoryStorage   settlementHistoryStorage

	currentState map[identity.Identity]settlementState
	settleQueue  chan receivedPromise
	stop         chan struct{}
	once         sync.Once
}

// HermesPromiseSettlerConfig configures the hermes promise settler accordingly.
type HermesPromiseSettlerConfig struct {
	HermesAddress        common.Address
	Threshold            float64
	MaxWaitForSettlement time.Duration
}

// NewHermesPromiseSettler creates a new instance of hermes promise settler.
func NewHermesPromiseSettler(eventBus eventbus.EventBus, transactor transactor, promiseStorage promiseStorage, providerChannelStatusProvider providerChannelStatusProvider, registrationStatusProvider registrationStatusProvider, ks ks, settlementHistoryStorage settlementHistoryStorage, config HermesPromiseSettlerConfig) *hermesPromiseSettler {
	return &hermesPromiseSettler{
		eventBus:                   eventBus,
		bc:                         providerChannelStatusProvider,
		ks:                         ks,
		registrationStatusProvider: registrationStatusProvider,
		config:                     config,
		currentState:               make(map[identity.Identity]settlementState),
		promiseStorage:             promiseStorage,
		settlementHistoryStorage:   settlementHistoryStorage,

		// defaulting to a queue of 5, in case we have a few active identities.
		settleQueue: make(chan receivedPromise, 5),
		stop:        make(chan struct{}),
		transactor:  transactor,
	}
}

// GetHermesFee fetches the hermes fee.
func (aps *hermesPromiseSettler) GetHermesFee(hermesID common.Address) (uint16, error) {
	return aps.bc.GetHermesFee(hermesID)
}

// loadInitialState loads the initial state for the given identity. Inteded to be called on service start.
func (aps *hermesPromiseSettler) loadInitialState(addr identity.Identity) error {
	aps.lock.Lock()
	defer aps.lock.Unlock()

	if _, ok := aps.currentState[addr]; ok {
		log.Info().Msgf("State for %v already loaded, skipping", addr)
		return nil
	}

	status, err := aps.registrationStatusProvider.GetRegistrationStatus(addr)
	if err != nil {
		return errors.Wrap(err, fmt.Sprintf("could not check registration status for %v", addr))
	}

	if status != registry.Registered {
		log.Info().Msgf("Provider %v not registered, skipping", addr)
		return nil
	}

	return aps.resyncState(addr, aps.config.HermesAddress)
}

func (aps *hermesPromiseSettler) resyncState(id identity.Identity, hermesID common.Address) error {
	channel, err := aps.bc.GetProviderChannel(hermesID, id.ToCommonAddress(), true)
	if err != nil {
		return errors.Wrap(err, fmt.Sprintf("could not get provider channel for %v, hermes %v", id, hermesID.Hex()))
	}

	hermesPromise, err := aps.promiseStorage.Get(id, hermesID)
	if err != nil && err != ErrNotFound {
		return errors.Wrap(err, fmt.Sprintf("could not get hermes promise for provider %v, hermes %v", id, hermesID.Hex()))
	}

	hs := hermesState{
		channel:     channel,
		lastPromise: hermesPromise.Promise,
	}

	s := aps.currentState[id]
	if len(s.hermeses) == 0 {
		s.hermeses = make(map[common.Address]hermesState)
	}
	s.registered = true
	s.hermeses[hermesID] = hs
	go aps.publishChangeEvent(id, aps.currentState[id], s)
	aps.currentState[id] = s
	log.Info().Msgf("Loaded state for provider %q, hermesID %q: balance %v, available balance %v, unsettled balance %v", id, hermesID.Hex(), hs.balance(), hs.availableBalance(), hs.unsettledBalance())
	return nil
}

func (aps *hermesPromiseSettler) publishChangeEvent(id identity.Identity, before, after settlementState) {
	aps.eventBus.Publish(event.AppTopicEarningsChanged, event.AppEventEarningsChanged{
		Identity: id,
		Previous: before.Earnings(),
		Current:  after.Earnings(),
	})
}

// Subscribe subscribes the hermes promise settler to the appropriate events
func (aps *hermesPromiseSettler) Subscribe() error {
	err := aps.eventBus.SubscribeAsync(nodevent.AppTopicNode, aps.handleNodeEvent)
	if err != nil {
		return errors.Wrap(err, "could not subscribe to node status event")
	}

	err = aps.eventBus.SubscribeAsync(registry.AppTopicIdentityRegistration, aps.handleRegistrationEvent)
	if err != nil {
		return errors.Wrap(err, "could not subscribe to registration event")
	}

	err = aps.eventBus.SubscribeAsync(servicestate.AppTopicServiceStatus, aps.handleServiceEvent)
	if err != nil {
		return errors.Wrap(err, "could not subscribe to service status event")
	}

	err = aps.eventBus.SubscribeAsync(event.AppTopicSettlementRequest, aps.handleSettlementEvent)
	if err != nil {
		return errors.Wrap(err, "could not subscribe to settlement event")
	}

	err = aps.eventBus.SubscribeAsync(event.AppTopicHermesPromise, aps.handleHermesPromiseReceived)
	return errors.Wrap(err, "could not subscribe to hermes promise event")
}

func (aps *hermesPromiseSettler) handleSettlementEvent(event event.AppEventSettlementRequest) {
	err := aps.ForceSettle(event.ProviderID, event.HermesID)
	if err != nil {
		log.Error().Err(err).Msg("could not settle promise")
	}
}

func (aps *hermesPromiseSettler) handleServiceEvent(event servicestate.AppEventServiceStatus) {
	switch event.Status {
	case string(servicestate.Running):
		err := aps.loadInitialState(identity.FromAddress(event.ProviderID))
		if err != nil {
			log.Error().Err(err).Msgf("could not load initial state for provider %v", event.ProviderID)
		}
	default:
		log.Debug().Msgf("Ignoring service event with status %v", event.Status)
	}
}

func (aps *hermesPromiseSettler) handleNodeEvent(payload nodevent.Payload) {
	if payload.Status == nodevent.StatusStarted {
		aps.handleNodeStart()
		return
	}

	if payload.Status == nodevent.StatusStopped {
		aps.handleNodeStop()
		return
	}
}

func (aps *hermesPromiseSettler) handleRegistrationEvent(payload registry.AppEventIdentityRegistration) {
	aps.lock.Lock()
	defer aps.lock.Unlock()

	if payload.Status != registry.Registered {
		log.Debug().Msgf("Ignoring event %v for provider %q", payload.Status.String(), payload.ID)
		return
	}
	log.Info().Msgf("Identity registration event received for provider %q", payload.ID)

	err := aps.resyncState(payload.ID, aps.config.HermesAddress)
	if err != nil {
		log.Error().Err(err).Msgf("Could not resync state for provider %v", payload.ID)
		return
	}

	log.Info().Msgf("Identity registration event handled for provider %q", payload.ID)
}

func (aps *hermesPromiseSettler) handleHermesPromiseReceived(apep event.AppEventHermesPromise) {
	id := apep.ProviderID
	log.Info().Msgf("Received hermes promise for %q", id)
	aps.lock.Lock()
	defer aps.lock.Unlock()

	s, ok := aps.currentState[apep.ProviderID]
	if !ok {
		log.Error().Msgf("Have no info on provider %q, skipping", id)
		return
	}
	if !s.registered {
		log.Error().Msgf("provider %q not registered, skipping", id)
		return
	}

	hermes, ok := s.hermeses[apep.HermesID]
	if !ok {
		err := aps.resyncState(id, apep.HermesID)
		if err != nil {
			log.Error().Err(err).Msgf("could not sync state for provider %v, hermesID %v", apep.ProviderID, apep.HermesID.Hex())
			return
		}
		hermes = s.hermeses[apep.HermesID]
	}

	hermes.lastPromise = apep.Promise
	s.hermeses[apep.HermesID] = hermes

	go aps.publishChangeEvent(id, aps.currentState[id], s)
	aps.currentState[apep.ProviderID] = s
	log.Info().Msgf("Hermes %q promise state updated for provider %q", apep.HermesID.Hex(), id)

	if s.needsSettling(aps.config.Threshold, apep.HermesID) {
		if hermes.channel.Stake != nil && hermes.channel.StakeGoal != nil && hermes.channel.Stake.Uint64() < hermes.channel.StakeGoal.Uint64() {
			go func() {
				err := aps.SettleIntoStake(id, apep.HermesID)
				log.Error().Err(err).Msgf("could not settle into stake for %q", apep.ProviderID)
			}()
		} else {
			aps.initiateSettling(apep.ProviderID, apep.HermesID)
		}
	}
}

func (aps *hermesPromiseSettler) initiateSettling(providerID identity.Identity, hermesID common.Address) {
	promise, err := aps.promiseStorage.Get(providerID, hermesID)
	if err == ErrNotFound {
		log.Debug().Msgf("no promise to settle for %q %q", providerID, hermesID.Hex())
		return
	}
	if err != nil {
		log.Error().Err(fmt.Errorf("could not get promise from storage: %w", err))
		return
	}

	hexR, err := hex.DecodeString(promise.R)
	if err != nil {
		log.Error().Err(fmt.Errorf("could encode R: %w", err))
		return
	}
	promise.Promise.R = hexR

	aps.settleQueue <- receivedPromise{
		hermesID: hermesID,
		provider: providerID,
		promise:  promise.Promise,
	}
}

func (aps *hermesPromiseSettler) listenForSettlementRequests() {
	log.Info().Msg("Listening for settlement events")
	defer func() {
		log.Info().Msg("Stopped listening for settlement events")
	}()

	for {
		select {
		case <-aps.stop:
			return
		case p := <-aps.settleQueue:
			go aps.settle(p, nil, false)
		}
	}
}

// GetEarnings returns current settlement status for given identity
func (aps *hermesPromiseSettler) GetEarnings(id identity.Identity) event.Earnings {
	aps.lock.RLock()
	defer aps.lock.RUnlock()

	return aps.currentState[id].Earnings()
}

// SettleIntoStake settles the promise but transfers the money to stake increase, not to beneficiary.
func (aps *hermesPromiseSettler) SettleIntoStake(providerID identity.Identity, hermesID common.Address) error {
	promise, err := aps.promiseStorage.Get(providerID, hermesID)
	if err == ErrNotFound {
		return ErrNothingToSettle
	}
	if err != nil {
		return errors.Wrap(err, "could not get promise from storage")
	}

	hexR, err := hex.DecodeString(promise.R)
	if err != nil {
		return errors.Wrap(err, "could not decode R")
	}
	promise.Promise.R = hexR
	return aps.settle(receivedPromise{
		promise:  promise.Promise,
		provider: providerID,
		hermesID: hermesID,
	}, nil, true)
}

// ErrNothingToSettle indicates that there is nothing to settle.
var ErrNothingToSettle = errors.New("nothing to settle for the given provider")

// ForceSettle forces the settlement for a provider
func (aps *hermesPromiseSettler) ForceSettle(providerID identity.Identity, hermesID common.Address) error {
	promise, err := aps.promiseStorage.Get(providerID, hermesID)
	if err == ErrNotFound {
		return ErrNothingToSettle
	}
	if err != nil {
		return errors.Wrap(err, "could not get promise from storage")
	}

	hexR, err := hex.DecodeString(promise.R)
	if err != nil {
		return errors.Wrap(err, "could not decode R")
	}

	promise.Promise.R = hexR
	return aps.settle(receivedPromise{
		promise:  promise.Promise,
		provider: providerID,
		hermesID: hermesID,
	}, nil, false)
}

// ForceSettle forces the settlement for a provider
func (aps *hermesPromiseSettler) SettleWithBeneficiary(providerID identity.Identity, beneficiary, hermesID common.Address) error {
	promise, err := aps.promiseStorage.Get(providerID, hermesID)
	fmt.Println(promise, err)
	if err == ErrNotFound {
		return ErrNothingToSettle
	}
	if err != nil {
		return errors.Wrap(err, "could not get promise from storage")
	}

	hexR, err := hex.DecodeString(promise.R)
	if err != nil {
		return errors.Wrap(err, "could not decode R")
	}

	promise.Promise.R = hexR
	return aps.settle(receivedPromise{
		promise:  promise.Promise,
		provider: providerID,
		hermesID: hermesID,
	}, &beneficiary, false)
}

// ErrSettleTimeout indicates that the settlement has timed out
var ErrSettleTimeout = errors.New("settle timeout")

func (aps *hermesPromiseSettler) settle(p receivedPromise, beneficiary *common.Address, isStakeIncrease bool) error {
	if aps.isSettling(p.provider) {
		return errors.New("provider already has settlement in progress")
	}

	aps.setSettling(p.provider, true)
	log.Info().Msgf("Marked provider %v as requesting settlement", p.provider)
	sink, cancel, err := aps.bc.SubscribeToPromiseSettledEvent(p.provider.ToCommonAddress(), p.hermesID)
	if err != nil {
		aps.setSettling(p.provider, false)
		log.Error().Err(err).Msg("Could not subscribe to promise settlement")
		return err
	}

	errCh := make(chan error)
	go func() {
		defer cancel()
		defer aps.setSettling(p.provider, false)
		defer close(errCh)
		select {
		case <-aps.stop:
			return
		case info, more := <-sink:
			if !more || info == nil {
				break
			}

			log.Info().Msgf("Settling complete for provider %v", p.provider)

			she := SettlementHistoryEntry{
				TxHash:       info.Raw.TxHash,
				Promise:      p.promise,
				Amount:       info.Amount,
				TotalSettled: info.TotalSettled,
			}
			if beneficiary != nil {
				she.Beneficiary = *beneficiary
			}

			err := aps.settlementHistoryStorage.Store(p.provider, aps.config.HermesAddress, she)
			if err != nil {
				log.Error().Err(err).Msgf("could not store settlement history")
			}

			err = aps.resyncState(p.provider, p.hermesID)
			if err != nil {
				// This will get retried so we do not need to explicitly retry
				// TODO: maybe add a sane limit of retries
				log.Error().Err(err).Msgf("Resync failed for provider %v", p.provider)
			} else {
				log.Info().Msgf("Resync success for provider %v", p.provider)
			}
			return
		case <-time.After(aps.config.MaxWaitForSettlement):
			log.Info().Msgf("Settle timeout for %v", p.provider)

			// send a signal to waiter that the settlement has timed out
			errCh <- ErrSettleTimeout
			return
		}
	}()

	var settleFunc = func() error {
		return aps.transactor.SettleAndRebalance(p.hermesID.Hex(), p.provider.Address, p.promise)
	}
	if isStakeIncrease {
		settleFunc = func() error {
			return aps.transactor.SettleIntoStake(p.hermesID.Hex(), p.provider.Address, p.promise)
		}
	} else if beneficiary != nil {
		settleFunc = func() error {
			return aps.transactor.SettleWithBeneficiary(p.provider.Address, beneficiary.Hex(), p.hermesID.Hex(), p.promise)
		}
	}

	err = settleFunc()
	if err != nil {
		cancel()
		log.Error().Err(err).Msgf("Could not settle promise for %v", p.provider.Address)
		return err
	}

	return <-errCh
}

func (aps *hermesPromiseSettler) isSettling(id identity.Identity) bool {
	aps.lock.RLock()
	defer aps.lock.RUnlock()
	v, ok := aps.currentState[id]
	if !ok {
		return false
	}

	return v.settleInProgress
}

func (aps *hermesPromiseSettler) setSettling(id identity.Identity, settling bool) {
	aps.lock.Lock()
	defer aps.lock.Unlock()
	v := aps.currentState[id]
	v.settleInProgress = settling
	aps.currentState[id] = v
}

func (aps *hermesPromiseSettler) handleNodeStart() {
	go aps.listenForSettlementRequests()

	for _, v := range aps.ks.Accounts() {
		addr := identity.FromAddress(v.Address.Hex())
		go func(address identity.Identity) {
			err := aps.loadInitialState(address)
			if err != nil {
				log.Error().Err(err).Msgf("could not load initial state for %v", addr)
			}
		}(addr)
	}
}

func (aps *hermesPromiseSettler) handleNodeStop() {
	aps.once.Do(func() {
		close(aps.stop)
	})
}

type hermesState struct {
	channel     client.ProviderChannel
	lastPromise crypto.Promise
}

// settlementState earning calculations model
type settlementState struct {
	settleInProgress bool
	registered       bool
	hermeses         map[common.Address]hermesState
}

// lifetimeBalance returns earnings of all history.
func (hs hermesState) lifetimeBalance() uint64 {
	return hs.lastPromise.Amount
}

// unsettledBalance returns current unsettled earnings.
func (hs hermesState) unsettledBalance() uint64 {
	settled := uint64(0)
	if hs.channel.Settled != nil {
		settled = hs.channel.Settled.Uint64()
	}

	return safeSub(hs.lastPromise.Amount, settled)
}

func (hs hermesState) availableBalance() uint64 {
	balance := uint64(0)
	if hs.channel.Balance != nil {
		balance = hs.channel.Balance.Uint64()
	}

	settled := uint64(0)
	if hs.channel.Settled != nil {
		settled = hs.channel.Settled.Uint64()
	}

	return balance + settled
}

func (hs hermesState) balance() uint64 {
	return safeSub(hs.availableBalance(), hs.lastPromise.Amount)
}

func (ss settlementState) needsSettling(threshold float64, hermesID common.Address) bool {
	if !ss.registered {
		return false
	}

	if ss.settleInProgress {
		return false
	}

	hermes, ok := ss.hermeses[hermesID]
	if !ok {
		return false
	}

	if hermes.channel.Stake.Uint64() == 0 {
		// if starting with zero stake, only settle one myst or more.
		if hermes.unsettledBalance() < 100000000 {
			return false
		}
	}

	calculatedThreshold := threshold * float64(hermes.availableBalance())
	possibleEarnings := float64(hermes.unsettledBalance())
	if possibleEarnings < calculatedThreshold {
		return false
	}

	if float64(hermes.balance()) <= calculatedThreshold {
		return true
	}

	return false
}

func (ss settlementState) Earnings() event.Earnings {
	var lifetimeBalance uint64
	var unsettledBalance uint64
	for _, v := range ss.hermeses {
		lifetimeBalance += v.lifetimeBalance()
		unsettledBalance += v.unsettledBalance()
	}
	return event.Earnings{
		LifetimeBalance:  lifetimeBalance,
		UnsettledBalance: unsettledBalance,
	}
}
