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
