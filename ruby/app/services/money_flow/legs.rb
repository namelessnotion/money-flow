# frozen_string_literal: true
# typed: strict

require_relative 'movement'

module Services
  module MoneyFlow
    # Each kind of Movement as a query over the business rows that record it,
    # all with the same five columns — kind, entity_id, security_id,
    # amount_minor_units, transaction_id — so they union into one.
    #
    # Every business row's id is the id of the Go Transaction it was sent as,
    # bar the Draw, whose id is on the Security.
    module Legs
      Kind = Movement::Kind

      sig { returns(Sequel::Dataset) }
      def self.union
        legs = [ach(Kind::Deposit), ach(Kind::Withdrawal), subscriptions, draws, repayments,
                disbursements(Kind::DisbursementPrincipal, :principal_minor_units),
                disbursements(Kind::DisbursementInterest, :interest_minor_units)]
        legs.drop(1).reduce(legs.fetch(0)) { |all, leg| all.union(leg, all: true, from_self: false) }
      end

      sig { params(kind: Kind).returns(Sequel::Dataset) }
      def self.ach(kind)
        DB[:ach_transactions]
          .where(direction: kind.serialize)
          .select(tag(kind), :entity_id, Sequel.cast(nil, :uuid).as(:security_id), :amount_minor_units,
                  Sequel[:id].as(:transaction_id))
      end
      private_class_method :ach

      sig { returns(Sequel::Dataset) }
      def self.subscriptions
        DB[:subscriptions].select(tag(Kind::Subscription), Sequel[:investor_entity_id].as(:entity_id), :security_id,
                                  :amount_minor_units, Sequel[:id].as(:transaction_id))
      end
      private_class_method :subscriptions

      # The whole offering moves at once, so the Draw is the Security's
      # principal.
      sig { returns(Sequel::Dataset) }
      def self.draws
        DB[:securities]
          .exclude(draw_transaction_id: nil)
          .select(tag(Kind::Draw), Sequel[:borrower_entity_id].as(:entity_id), Sequel[:id].as(:security_id),
                  Sequel[:principal_minor_units].as(:amount_minor_units),
                  Sequel[:draw_transaction_id].as(:transaction_id))
      end
      private_class_method :draws

      # One leg collects principal and interest as a single amount.
      sig { returns(Sequel::Dataset) }
      def self.repayments
        repayment = Sequel[:repayments]
        DB[:repayments]
          .join(:securities, id: :security_id)
          .select(tag(Kind::Repayment), Sequel[:securities][:borrower_entity_id].as(:entity_id),
                  repayment[:security_id],
                  (repayment[:principal_minor_units] + repayment[:interest_minor_units]).as(:amount_minor_units),
                  repayment[:id].as(:transaction_id))
      end
      private_class_method :repayments

      # A part that moved nothing — the principal of an interest-only share —
      # is left out rather than drawn as an edge of zero. Neither part is ever
      # negative (the table's own check), so "not zero" is "moved something".
      sig { params(kind: Kind, part: Symbol).returns(Sequel::Dataset) }
      def self.disbursements(kind, part)
        disbursement = Sequel[:disbursements]
        DB[:disbursements]
          .join(:repayments, id: :repayment_id)
          .exclude(disbursement[part] => 0)
          .select(tag(kind), disbursement[:investor_entity_id].as(:entity_id), Sequel[:repayments][:security_id],
                  disbursement[part].as(:amount_minor_units), disbursement[:id].as(:transaction_id))
      end
      private_class_method :disbursements

      sig { params(kind: Kind).returns(Sequel::SQL::AliasedExpression) }
      def self.tag(kind)
        Sequel.cast(kind.serialize, :text).as(:kind)
      end
      private_class_method :tag
    end
  end
end
