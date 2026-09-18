# frozen_string_literal: true
# typed: strict

require_relative '../../../gen/proto/transaction/v1/transaction_pb'
require_relative '../../../gen/proto/transaction/v1/transaction_twirp'
require_relative '../../../gen/proto/transfer/v1/transfer_pb'
require_relative '../../../gen/proto/transfer/v1/transfer_twirp'
require_relative '../twirp_call'
require_relative 'errors'

module Services
  module Ach
    # The Go commands an ACH Transaction's life needs, in its own terms.
    #
    # Each call goes through TwirpCall's retry policy — every one is
    # idempotent per id in Go — and unwraps Go's domain refusal, which arrives
    # inside a successful response, into Refused.
    #
    # The RPC methods are defined by protoc-gen-twirp_ruby's `rpc` DSL at load
    # time (define_method), which Sorbet can't see statically, hence T.unsafe
    # on each client call. Their requests and responses are generated
    # messages, which Sorbet sees as values rather than classes.
    class GoGateway
      DEFAULT_URL = 'http://localhost:8080/twirp'

      # Where a Transaction stands once Go has run its saga as far as it can.
      class Outcome < T::Struct
        # A Transaction::V1::TransactionState enum name, e.g. :TRANSACTION_STATE_STARTED.
        const :state, Symbol
        # Why, when Go gave a reason; empty otherwise.
        const :reason, String

        sig { returns(T::Boolean) }
        def started? = state == :TRANSACTION_STATE_STARTED
      end

      sig do
        params(
          transaction_client: Transaction::V1::TransactionServiceClient,
          transfer_client: Transfer::V1::TransferServiceClient
        ).void
      end
      def initialize(
        transaction_client: Transaction::V1::TransactionServiceClient.new(
          ENV.fetch('TRANSACTION_SERVICE_URL', DEFAULT_URL)
        ),
        transfer_client: Transfer::V1::TransferServiceClient.new(ENV.fetch('TRANSFER_SERVICE_URL', DEFAULT_URL))
      )
        @transaction_client = transaction_client
        @transfer_client = transfer_client
      end

      # Asks Go to run the Transaction `request` describes. Idempotent by the
      # request's id: Go returns the decision it already recorded.
      sig { params(request: T.untyped).void }
      def start_transaction(request)
        response = retrying { T.unsafe(@transaction_client).start_initializing_transaction(request) }
        refused!(response.data&.transaction_rejected)
      end

      # Submission: the provider has the entry, so the staged Transfer waits,
      # pending, for the ACH network.
      sig { params(transfer_id: String).void }
      def confirm_staged(transfer_id)
        request = Transfer::V1::ConfirmStagedTransferRequest.new(id: transfer_id)
        response = retrying { T.unsafe(@transfer_client).confirm_staged_transfer(request) }
        refused!(response.data&.confirm_staged_transfer_rejected)
      end

      # Settlement: the ACH network posted the entry.
      sig { params(transfer_id: String).void }
      def post_pending(transfer_id)
        request = Transfer::V1::PostPendingTransferRequest.new(id: transfer_id)
        response = retrying { T.unsafe(@transfer_client).post_pending_transfer(request) }
        refused!(response.data&.post_pending_transfer_rejected)
      end

      # A return, or a refused submission: the entry will never post.
      sig { params(transfer_id: String, reason: String).void }
      def cancel_staged(transfer_id, reason)
        request = Transfer::V1::CancelStagedTransferRequest.new(id: transfer_id, reason: reason)
        response = retrying { T.unsafe(@transfer_client).cancel_staged_transfer(request) }
        refused!(response.data&.cancel_staged_transfer_rejected)
      end

      # Runs the Transaction's saga forward from what its Transfers now say —
      # the shadow leg after a settlement, the rollback after a return — and
      # answers where it then stands.
      sig { params(transaction_id: String).returns(Outcome) }
      def resume(transaction_id)
        request = Transaction::V1::ResumeTransactionRequest.new(id: transaction_id)
        data = retrying { T.unsafe(@transaction_client).resume_transaction(request) }.data
        Outcome.new(state: data.state, reason: data.reason)
      end

      private

      sig { params(rpc: T.proc.returns(Twirp::ClientResp)).returns(Twirp::ClientResp) }
      def retrying(&rpc)
        TwirpCall.with_retries(failure: Unavailable) { rpc.call }
      end

      # `rejection` is whichever generated *Rejected message the response's
      # oneof carried, if any; each has a `reason`.
      sig { params(rejection: T.untyped).void }
      def refused!(rejection)
        raise Refused, rejection.reason if rejection
      end
    end
  end
end
