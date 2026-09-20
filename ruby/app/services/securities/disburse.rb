# frozen_string_literal: true
# typed: strict

require_relative '../base_service'
require_relative '../det_id'
require_relative 'allocation'
require_relative 'disbursement_shape'
require_relative 'errors'
require_relative 'go_gateway'

module Services
  module Securities
    # Pays one holder their share of one Repayment, retiring that much of their
    # claim on the way.
    #
    # One Transaction per holder rather than one fan-out across all of them
    # (ruby/docs/adr/0007). Its ids are derived from the Repayment and the
    # Investor, so the sweep re-sending one is a no-op in Go and Ruby keeps no
    # "already disbursed" flag — the row is a record of what was sent.
    #
    # Unlike the other services here it does not resume: the sweep needs no
    # synchronous answer, and the projection tells the story. A Disbursement
    # that rolls back is visible and needs a person, not a retry.
    class Disburse < BaseService
      sig { params(gateway: GoGateway).void }
      def initialize(gateway: GoGateway.new)
        super()
        @go = gateway
      end

      sig do
        params(repayment: Models::Repayment, share: Allocation::Share).returns(Models::Disbursement)
      end
      def call(repayment:, share:)
        security = Allocation.security_of(repayment)
        ids = ids_for(repayment, share)

        disbursement = perform { record(repayment, share, ids) }
        start_transaction!(security, share, disbursement)
        disbursement
      end

      # The Transaction id for one holder's share of one Repayment. Public so
      # the sweep can ask whether the read model has already seen it without
      # building anything.
      sig { params(repayment_id: String, investor_entity_id: Integer).returns(String) }
      def self.transaction_id(repayment_id, investor_entity_id)
        DetId.for("#{repayment_id}:disbursement:#{investor_entity_id}")
      end

      private

      # A Disbursement's ids, all derived from the pair they belong to. The
      # retirement id is absent when there is no principal to retire: Go turns
      # a zero-amount Transfer into a transport error its saga swallows, so
      # that leg must not exist rather than be zero.
      sig { params(repayment: Models::Repayment, share: Allocation::Share).returns(T::Hash[Symbol, T.nilable(String)]) }
      def ids_for(repayment, share)
        seed = "#{repayment.id}:disbursement:#{share.investor_entity_id}"
        {
          transaction: self.class.transaction_id(repayment.id, share.investor_entity_id),
          payout: DetId.for("#{seed}:payout"),
          retirement: share.principal_minor_units.positive? ? DetId.for("#{seed}:retirement") : nil
        }
      end

      sig do
        params(repayment: Models::Repayment, share: Allocation::Share, ids: T::Hash[Symbol, T.nilable(String)])
          .returns(Models::Disbursement)
      end
      def record(repayment, share, ids)
        transaction_id = ids.fetch(:transaction)
        # A row already here is what a previous run sent, not a flag saying it
        # is done — Go is the guard. Re-sending is a no-op there.
        Models::Disbursement[transaction_id] || Models::Disbursement.create(
          **attributes(repayment, share, ids), id: transaction_id
        )
      end

      sig do
        params(repayment: Models::Repayment, share: Allocation::Share, ids: T::Hash[Symbol, T.nilable(String)])
          .returns(T::Hash[Symbol, T.untyped])
      end
      def attributes(repayment, share, ids)
        {
          repayment_id: repayment.id,
          investor_entity_id: share.investor_entity_id,
          principal_minor_units: share.principal_minor_units,
          interest_minor_units: share.interest_minor_units,
          currency: repayment.currency,
          retirement_transfer_id: ids.fetch(:retirement),
          payout_transfer_id: ids.fetch(:payout)
        }
      end

      sig do
        params(security: Models::Security, share: Allocation::Share, disbursement: Models::Disbursement).void
      end
      def start_transaction!(security, share, disbursement)
        @go.start_transaction(
          shape_for(security, share, disbursement).start_request(
            transaction_id: disbursement.id,
            principal_minor_units: disbursement.principal_minor_units,
            interest_minor_units: disbursement.interest_minor_units
          )
        )
      end

      sig do
        params(security: Models::Security, share: Allocation::Share, disbursement: Models::Disbursement)
          .returns(DisbursementShape)
      end
      def shape_for(security, share, disbursement)
        accounts = Models::Account.where(entity_id: [security.issuer_entity_id, share.investor_entity_id]).all
        DisbursementShape.for(
          security: security, investor_entity_id: share.investor_entity_id, accounts: accounts,
          payout_transfer_id: disbursement.payout_transfer_id,
          retirement_transfer_id: disbursement.retirement_transfer_id
        )
      end
    end
  end
end
