# frozen_string_literal: true
# typed: strict

require_relative 'securities_mutation'
require_relative '../types/objects/subscription'

module Mutations
  # Buys a fraction of a Security for an Investor.
  class PurchaseSecurity < SecuritiesMutation
    description 'Buy a fraction of a Security. Refused if the Supply is short, or if the Investor’s cleared ' \
                'cash is — the latter only once Go has run and rolled the Transaction back.'

    argument :security_id, ID, required: true
    argument :investor_entity_id, ID, required: true
    argument :amount_minor_units, GraphQL::Types::BigInt, required: true

    field :subscription, Types::Subscription, null: true

    sig do
      params(security_id: String, investor_entity_id: String, amount_minor_units: Integer)
        .returns(T::Hash[Symbol, T.untyped])
    end
    def resolve(security_id:, investor_entity_id:, amount_minor_units:)
      answering do
        subscription = Services::Securities::Purchase.new.call(
          security_id: security_id, investor_entity_id: Integer(investor_entity_id, 10),
          amount_minor_units: amount_minor_units
        )
        { subscription: Types::Subscription.dataset.where(Sequel[:subscriptions][:id] => subscription.id).first }
      end
    end
  end
end
