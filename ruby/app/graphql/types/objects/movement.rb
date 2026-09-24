# frozen_string_literal: true
# typed: strict

require_relative 'base_object'
require_relative '../enums/movement_kind_enum'

module Types
  # GraphQL type for one Services::MoneyFlow::Movement.
  class Movement < BaseObject
    description 'One completed Transaction’s money, from one Party to another. A Disbursement is two: the ' \
                'principal it returns and the interest it pays.'

    field :kind, MovementKindEnum, null: false
    field :source, ID, null: false, description: 'The Party the money left.'
    field :target, ID, null: false, description: 'The Party the money reached.'
    field :amount_minor_units, GraphQL::Types::BigInt, null: false
    field :occurred_at, GraphQL::Types::ISO8601DateTime, null: false,
                                                         description: 'When Go says the Transaction completed.'
    field :transaction_id, ID, null: false, description: 'The Go Transaction that moved it.'
  end
end
