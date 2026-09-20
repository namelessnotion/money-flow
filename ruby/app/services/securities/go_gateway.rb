# frozen_string_literal: true
# typed: strict

require_relative '../../../gen/proto/holder/v1/holder_pb'
require_relative '../../../gen/proto/holder/v1/holder_twirp'
require_relative '../../../gen/proto/transaction/v1/transaction_pb'
require_relative '../../../gen/proto/transaction/v1/transaction_twirp'
require_relative '../twirp_call'
require_relative 'errors'

module Services
  module Securities
    # The Go commands a Security's life needs, in its own terms.
    #
    # Each call goes through TwirpCall's retry policy — every one is idempotent
    # per id in Go — and unwraps Go's domain refusal, which arrives inside a
    # successful response, into Refused.
    #
    # Deliberately separate from Services::Ach::GoGateway rather than shared:
    # the repo keeps one gateway per capability, this one needs a verb ACH has
    # no use for, and none of ACH's settlement verbs apply here because nothing
    # a Security does crosses the bank boundary or stages.
    #
    # The RPC methods are defined by protoc-gen-twirp_ruby's `rpc` DSL at load
    # time (define_method), which Sorbet can't see statically, hence T.unsafe
    # on each client call. Their requests and responses are generated messages,
    # which Sorbet sees as values rather than classes.
    class GoGateway
      DEFAULT_URL = 'http://localhost:8080/twirp'

      # Where a Transaction stands once Go has run its saga as far as it can.
      class Outcome < T::Struct
        # A Transaction::V1::TransactionState enum name, e.g. :TRANSACTION_STATE_COMPLETED.
        const :state, Symbol
        # Why, when Go gave a reason; empty otherwise.
        const :reason, String

        sig { returns(T::Boolean) }
        def started? = state == :TRANSACTION_STATE_STARTED

        # Nothing a Security originates stages, so the whole DAG normally runs
        # to completion inside the call that started it.
        sig { returns(T::Boolean) }
        def completed? = state == :TRANSACTION_STATE_COMPLETED

        # Rejected outright, or rolling back, or rolled back, or stuck partway
        # through a rollback. Every one of these means the command did not do
        # what was asked, and the last needs a person.
        FAILED = T.let(
          %i[
            TRANSACTION_STATE_REJECTED
            TRANSACTION_STATE_ROLLBACK_STARTED
            TRANSACTION_STATE_ROLLED_BACK
            TRANSACTION_STATE_ROLLBACK_FAILED
          ].freeze,
          T::Array[Symbol]
        )

        sig { returns(T::Boolean) }
        def failed? = FAILED.include?(state)
      end

      sig do
        params(
          transaction_client: Transaction::V1::TransactionServiceClient,
          holder_client: Holder::V1::HolderServiceClient
        ).void
      end
      def initialize(
        transaction_client: Transaction::V1::TransactionServiceClient.new(
          ENV.fetch('TRANSACTION_SERVICE_URL', DEFAULT_URL)
        ),
        holder_client: Holder::V1::HolderServiceClient.new(ENV.fetch('HOLDER_SERVICE_URL', DEFAULT_URL))
      )
        @transaction_client = transaction_client
        @holder_client = holder_client
      end

      # Asks Go to run the Transaction `request` describes. Idempotent by the
      # request's id: Go returns the decision it already recorded.
      sig { params(request: T.untyped).void }
      def start_transaction(request)
        response = retrying { T.unsafe(@transaction_client).start_initializing_transaction(request) }
        refused!(response.data&.transaction_rejected)
      end

      # Runs the Transaction's saga forward from what its Transfers now say, and
      # answers where it then stands.
      #
      # This matters more here than it does for ACH. A Security's shapes gate
      # their second leg behind their first, and Go's accept-time pre-flight
      # only looks at children ready at time zero — so a gated leg's real
      # outcome is only ever learned by asking.
      sig { params(transaction_id: String).returns(Outcome) }
      def resume(transaction_id)
        request = Transaction::V1::ResumeTransactionRequest.new(id: transaction_id)
        data = retrying { T.unsafe(@transaction_client).resume_transaction(request) }.data
        Outcome.new(state: data.state, reason: data.reason)
      end

      # Opens `specs` as Wallets on a Holder that already exists — a Security's
      # three, on its Issuer's.
      #
      # Provision rather than three AddWallets: it is idempotent including
      # partially, appending only what is missing when the stream is already
      # there, so a retry after an ambiguous failure converges instead of
      # conflicting.
      sig { params(holder_uuid: String, specs: T::Array[T.untyped]).void }
      def provision_wallets(holder_uuid, specs)
        request = Holder::V1::ProvisionRequest.new(id: holder_uuid, wallets: specs)
        response = retrying { T.unsafe(@holder_client).provision(request) }
        refused!(response.data&.holder_provision_rejected)
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
