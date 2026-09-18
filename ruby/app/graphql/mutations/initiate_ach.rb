# frozen_string_literal: true
# typed: strict

require_relative 'ach_mutation'

module Mutations
  # Shared by initiateAchDeposit and initiateAchWithdrawal, which differ only in
  # direction.
  class InitiateAch < AchMutation
    argument :entity_id, ID, required: true
    argument :amount_minor_units, GraphQL::Types::BigInt, required: true, description: 'Amount in US cents.'

    sig { params(entity_id: String, amount_minor_units: Integer).returns(T::Hash[Symbol, T.untyped]) }
    def resolve(entity_id:, amount_minor_units:)
      answering do
        Services::Ach::Initiate.new.call(
          request: InitiateAchRequest.new(entity_id: Integer(entity_id, 10), direction: direction,
                                          amount_minor_units: amount_minor_units)
        )
      end
    end

    private

    sig { returns(Types::Enums::AchDirection) }
    def direction
      raise NotImplementedError, "#{self.class} must name its direction"
    end
  end

  # Pulls money in from the entity's bank account.
  class InitiateAchDeposit < InitiateAch
    description 'Originate an ACH debit of the entity’s bank account into its cash account.'

    private

    sig { override.returns(Types::Enums::AchDirection) }
    def direction = Types::Enums::AchDirection::Deposit
  end

  # Pushes money out to the entity's bank account.
  class InitiateAchWithdrawal < InitiateAch
    description 'Originate an ACH credit from the entity’s cash account out to its bank account.'

    private

    sig { override.returns(Types::Enums::AchDirection) }
    def direction = Types::Enums::AchDirection::Withdrawal
  end
end
