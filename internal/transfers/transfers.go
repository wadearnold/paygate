// Copyright 2020 The Moov Authors
// Use of this source code is governed by an Apache License
// license that can be found in the LICENSE file.

package transfers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"

	"github.com/moov-io/ach"
	"github.com/moov-io/base"
	"github.com/moov-io/base/idempotent"
	"github.com/moov-io/paygate/internal/accounts"
	"github.com/moov-io/paygate/internal/customers"
	"github.com/moov-io/paygate/internal/depository"
	"github.com/moov-io/paygate/internal/events"
	"github.com/moov-io/paygate/internal/model"
	"github.com/moov-io/paygate/internal/originators"
	"github.com/moov-io/paygate/internal/receivers"
	"github.com/moov-io/paygate/internal/remoteach"
	"github.com/moov-io/paygate/internal/route"
	"github.com/moov-io/paygate/pkg/achclient"
	"github.com/moov-io/paygate/pkg/id"

	"github.com/go-kit/kit/log"
	"github.com/gorilla/mux"
)

type transferRequest struct {
	Type                   model.TransferType `json:"transferType"`
	Amount                 model.Amount       `json:"amount"`
	Originator             model.OriginatorID `json:"originator"`
	OriginatorDepository   id.Depository      `json:"originatorDepository"`
	Receiver               model.ReceiverID   `json:"receiver"`
	ReceiverDepository     id.Depository      `json:"receiverDepository"`
	Description            string             `json:"description,omitempty"`
	StandardEntryClassCode string             `json:"standardEntryClassCode"`
	SameDay                bool               `json:"sameDay,omitempty"`

	CCDDetail *model.CCDDetail `json:"CCDDetail,omitempty"`
	IATDetail *model.IATDetail `json:"IATDetail,omitempty"`
	TELDetail *model.TELDetail `json:"TELDetail,omitempty"`
	WEBDetail *model.WEBDetail `json:"WEBDetail,omitempty"`

	// Internal fields for auditing and tracing
	fileID        string
	transactionID string
	remoteAddr    string
	userID        id.User
}

func (r transferRequest) missingFields() error {
	var missing []string
	check := func(name, s string) {
		if s == "" {
			missing = append(missing, name)
		}
	}

	check("transferType", string(r.Type))
	check("originator", string(r.Originator))
	check("originatorDepository", string(r.OriginatorDepository))
	check("receiver", string(r.Receiver))
	check("receiverDepository", string(r.ReceiverDepository))
	check("standardEntryClassCode", string(r.StandardEntryClassCode))

	if len(missing) > 0 {
		return fmt.Errorf("missing %s JSON field(s)", strings.Join(missing, ", "))
	}
	return nil
}

func (r transferRequest) asTransfer(transferID string) *model.Transfer {
	xfer := &model.Transfer{
		ID:                     id.Transfer(transferID),
		Type:                   r.Type,
		Amount:                 r.Amount,
		Originator:             r.Originator,
		OriginatorDepository:   r.OriginatorDepository,
		Receiver:               r.Receiver,
		ReceiverDepository:     r.ReceiverDepository,
		Description:            r.Description,
		StandardEntryClassCode: r.StandardEntryClassCode,
		Status:                 model.TransferPending,
		SameDay:                r.SameDay,
		Created:                base.Now(),
		UserID:                 r.userID.String(),
	}
	// Copy along the YYYDetail sub-object for specific SEC codes
	// where we expect one in the JSON request body.
	switch xfer.StandardEntryClassCode {
	case ach.CCD:
		xfer.CCDDetail = r.CCDDetail
	case ach.IAT:
		xfer.IATDetail = r.IATDetail
	case ach.TEL:
		xfer.TELDetail = r.TELDetail
	case ach.WEB:
		xfer.WEBDetail = r.WEBDetail
	}
	return xfer
}

type TransferRouter struct {
	logger log.Logger

	depRepo            depository.Repository
	eventRepo          events.Repository
	receiverRepository receivers.Repository
	origRepo           originators.Repository
	transferRepo       Repository

	transferLimitChecker *LimitChecker

	achClientFactory func(userID id.User) *achclient.ACH

	accountsClient  accounts.Client
	customersClient customers.Client
}

func NewTransferRouter(
	logger log.Logger,
	depositoryRepo depository.Repository,
	eventRepo events.Repository,
	receiverRepo receivers.Repository,
	originatorsRepo originators.Repository,
	transferRepo Repository,
	transferLimitChecker *LimitChecker,
	achClientFactory func(userID id.User) *achclient.ACH,
	accountsClient accounts.Client,
	customersClient customers.Client,
) *TransferRouter {
	return &TransferRouter{
		logger:               logger,
		depRepo:              depositoryRepo,
		eventRepo:            eventRepo,
		receiverRepository:   receiverRepo,
		origRepo:             originatorsRepo,
		transferRepo:         transferRepo,
		transferLimitChecker: transferLimitChecker,
		achClientFactory:     achClientFactory,
		accountsClient:       accountsClient,
		customersClient:      customersClient,
	}
}

func (c *TransferRouter) RegisterRoutes(router *mux.Router) {
	router.Methods("GET").Path("/transfers").HandlerFunc(c.getUserTransfers())
	router.Methods("GET").Path("/transfers/{transferId}").HandlerFunc(c.getUserTransfer())

	router.Methods("POST").Path("/transfers").HandlerFunc(c.createUserTransfers())
	router.Methods("POST").Path("/transfers/batch").HandlerFunc(c.createUserTransfers())

	router.Methods("DELETE").Path("/transfers/{transferId}").HandlerFunc(c.deleteUserTransfer())

	router.Methods("GET").Path("/transfers/{transferId}/events").HandlerFunc(c.getUserTransferEvents())
	router.Methods("POST").Path("/transfers/{transferId}/failed").HandlerFunc(c.validateUserTransfer())
	router.Methods("POST").Path("/transfers/{transferId}/files").HandlerFunc(c.getUserTransferFiles())
}

func getTransferID(r *http.Request) id.Transfer {
	vars := mux.Vars(r)
	v, ok := vars["transferId"]
	if ok {
		return id.Transfer(v)
	}
	return id.Transfer("")
}

func (c *TransferRouter) getUserTransfers() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		responder := route.NewResponder(c.logger, w, r)
		if responder == nil {
			return
		}

		transfers, err := c.transferRepo.getUserTransfers(responder.XUserID)
		if err != nil {
			responder.Log("transfers", fmt.Sprintf("error getting user transfers: %v", err))
			responder.Problem(err)
			return
		}

		responder.Respond(func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(transfers)
		})
	}
}

func (c *TransferRouter) getUserTransfer() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		responder := route.NewResponder(c.logger, w, r)
		if responder == nil {
			return
		}

		transferID := getTransferID(r)
		transfer, err := c.transferRepo.getUserTransfer(transferID, responder.XUserID)
		if err != nil {
			responder.Log("transfers", fmt.Sprintf("error reading transfer=%s: %v", transferID, err))
			responder.Problem(err)
			return
		}

		responder.Respond(func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(transfer)
		})
	}
}

// readTransferRequests will attempt to parse the incoming body as either a transferRequest or []transferRequest.
// If no requests were read a non-nil error is returned.
func readTransferRequests(r *http.Request) ([]*transferRequest, error) {
	bs, err := ioutil.ReadAll(route.Read(r.Body))
	if err != nil {
		return nil, err
	}

	var req transferRequest
	var requests []*transferRequest
	if err := json.Unmarshal(bs, &req); err != nil {
		// failed, but try []transferRequest
		if err := json.Unmarshal(bs, &requests); err != nil {
			return nil, err
		}
	} else {
		if err := req.missingFields(); err != nil {
			return nil, err
		}
		requests = append(requests, &req)
	}
	if len(requests) == 0 {
		return nil, errors.New("no Transfer request objects found")
	}
	return requests, nil
}

func (c *TransferRouter) createUserTransfers() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		responder := route.NewResponder(c.logger, w, r)
		if responder == nil {
			return
		}

		requests, err := readTransferRequests(r)
		if err != nil {
			responder.Problem(err)
			return
		}

		achClient := c.achClientFactory(responder.XUserID)

		// Carry over any incoming idempotency key and set one otherwise
		idempotencyKey := idempotent.Header(r)
		if idempotencyKey == "" {
			idempotencyKey = base.ID()
		}
		remoteIP := route.RemoteAddr(r.Header)

		for i := range requests {
			transferID, req := base.ID(), requests[i]
			if err := req.missingFields(); err != nil {
				responder.Problem(err)
				return
			}
			requests[i].remoteAddr = remoteIP
			requests[i].userID = responder.XUserID

			// Grab and validate objects required for this transfer.
			receiver, receiverDep, orig, origDep, err := getTransferObjects(req, responder.XUserID, c.depRepo, c.receiverRepository, c.origRepo)
			if err != nil {
				objects := fmt.Sprintf("receiver=%v, receiverDep=%v, orig=%v, origDep=%v, err: %v", receiver, receiverDep, orig, origDep, err)
				responder.Log("transfers", fmt.Sprintf("Unable to find all objects during transfer create for user_id=%s, %s", responder.XUserID, objects))

				// Respond back to user
				responder.Problem(fmt.Errorf("missing data to create transfer: %s", err))
				return
			}

			// Check limits for this userID and destination
			// TODO(adam): We'll need user level limit overrides
			if err := c.transferLimitChecker.allowTransfer(responder.XUserID); err != nil {
				responder.Log("transfers", fmt.Sprintf("rejecting transfers: %v", err))
				responder.Problem(err)
				return
			}

			// Post the Transfer's transaction against the Accounts
			var transactionID string
			if c.accountsClient != nil {
				tx, err := c.postAccountTransaction(responder.XUserID, origDep, receiverDep, req.Amount, req.Type, responder.XRequestID)
				if err != nil {
					responder.Log("transfers", err.Error())
					responder.Problem(err)
					return
				}
				transactionID = tx.ID
			}

			// Verify Customer statuses related to this transfer
			if c.customersClient != nil {
				if err := verifyCustomerStatuses(orig, receiver, c.customersClient, responder.XRequestID, responder.XUserID); err != nil {
					responder.Log("transfers", "problem with Customer checks", "error", err.Error())
					responder.Problem(err)
					return
				} else {
					responder.Log("transfers", "Customer check passed")
				}

				// Check disclaimers for Originator and Receiver
				if err := verifyDisclaimersAreAccepted(orig, receiver, c.customersClient, responder.XRequestID, responder.XUserID); err != nil {
					responder.Log("transfers", "problem with disclaimers", "error", err.Error())
					responder.Problem(err)
					return
				} else {
					responder.Log("transfers", "Disclaimer checks passed")
				}
			}

			// Save Transfer object
			transfer := req.asTransfer(transferID)
			file, err := remoteach.ConstructFile(transferID, idempotencyKey, transfer, receiver, receiverDep, orig, origDep)
			if err != nil {
				responder.Problem(err)
				return
			}
			fileID, err := achClient.CreateFile(idempotencyKey, file)
			if err != nil {
				responder.Problem(err)
				return
			}
			if err := remoteach.CheckFile(c.logger, achClient, fileID, responder.XUserID); err != nil {
				responder.Problem(err)
				return
			}

			// Add internal ID's (fileID, transaction.ID) onto our request so we can store them in our database
			req.fileID = fileID
			req.transactionID = transactionID

			// Write events for our audit/history log
			if err := writeTransferEvent(responder.XUserID, req, c.eventRepo); err != nil {
				responder.Log("transfers", fmt.Sprintf("error writing transfer=%s event: %v", transferID, err))
				responder.Problem(err)
				return
			}
		}

		// TODO(adam): We still create Transfers if the micro-deposits have been confirmed, but not merged (and uploaded)
		// into an ACH file. Should we check that case in this method and reject Transfers whose Depositories micro-deposts
		// haven't even been merged yet?

		transfers, err := c.transferRepo.createUserTransfers(responder.XUserID, requests)
		if err != nil {
			responder.Log("transfers", fmt.Sprintf("error creating transfers: %v", err))
			responder.Problem(err)
			return
		}

		writeResponse(c.logger, w, len(requests), transfers)
		responder.Log("transfers", fmt.Sprintf("Created transfers for user_id=%s request=%s", responder.XUserID, responder.XRequestID))
	}
}

func (c *TransferRouter) deleteUserTransfer() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		responder := route.NewResponder(c.logger, w, r)
		if responder == nil {
			return
		}

		transferID := getTransferID(r)
		transfer, err := c.transferRepo.getUserTransfer(transferID, responder.XUserID)
		if err != nil {
			responder.Log("transfers", fmt.Sprintf("error reading transfer=%s for deletion: %v", transferID, err))
			responder.Problem(err)
			return
		}
		if transfer.Status != model.TransferPending {
			responder.Problem(fmt.Errorf("a %s transfer can't be deleted", transfer.Status))
			return
		}

		// Delete from our database
		if err := c.transferRepo.deleteUserTransfer(transferID, responder.XUserID); err != nil {
			responder.Problem(err)
			return
		}

		// Delete from our ACH service
		fileID, err := c.transferRepo.GetFileIDForTransfer(transferID, responder.XUserID)
		if err != nil && err != sql.ErrNoRows {
			responder.Problem(err)
			return
		}
		if fileID != "" {
			if err := c.achClientFactory(responder.XUserID).DeleteFile(fileID); err != nil {
				responder.Problem(err)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
	}
}

// getTransferObjects performs database lookups to grab all the objects needed to make a transfer.
//
// This method also verifies the status of the Receiver, Receiver Depository and Originator Repository
//
// All return values are either nil or non-nil and the error will be the opposite.
func getTransferObjects(req *transferRequest, userID id.User, depRepo depository.Repository, receiverRepository receivers.Repository, origRepo originators.Repository) (*model.Receiver, *model.Depository, *model.Originator, *model.Depository, error) {
	// Receiver
	receiver, err := receiverRepository.GetUserReceiver(req.Receiver, userID)
	if err != nil {
		return nil, nil, nil, nil, errors.New("receiver not found")
	}
	if err := receiver.Validate(); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("receiver: %v", err)
	}

	receiverDep, err := depRepo.GetUserDepository(req.ReceiverDepository, userID)
	if err != nil {
		return nil, nil, nil, nil, errors.New("receiver depository not found")
	}
	if receiverDep.Status != model.DepositoryVerified {
		return nil, nil, nil, nil, fmt.Errorf("receiver depository %s is in status %v", receiverDep.ID, receiverDep.Status)
	}
	if err := receiverDep.Validate(); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("receiver depository: %v", err)
	}

	// Originator
	orig, err := origRepo.GetUserOriginator(req.Originator, userID)
	if err != nil {
		return nil, nil, nil, nil, errors.New("originator not found")
	}
	if err := orig.Validate(); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("originator: %v", err)
	}

	origDep, err := depRepo.GetUserDepository(req.OriginatorDepository, userID)
	if err != nil {
		return nil, nil, nil, nil, errors.New("originator Depository not found")
	}
	if origDep.Status != model.DepositoryVerified {
		return nil, nil, nil, nil, fmt.Errorf("originator Depository %s is in status %v", origDep.ID, origDep.Status)
	}
	if err := origDep.Validate(); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("originator depository: %v", err)
	}

	return receiver, receiverDep, orig, origDep, nil
}

func writeResponse(logger log.Logger, w http.ResponseWriter, reqCount int, transfers []*model.Transfer) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)

	if reqCount == 1 {
		// don't render surrounding array for single transfer create
		// (it's coming from POST /transfers, not POST /transfers/batch)
		json.NewEncoder(w).Encode(transfers[0])
	} else {
		json.NewEncoder(w).Encode(transfers)
	}
}
