# frozen_string_literal: true
# typed: strict

module Types
  # The terms of an offering, as one argument.
  #
  # An input object rather than six flat arguments: they are one thing — what
  # is being offered and on what terms — and a caller that passes a rate where
  # a term belongs is the failure mode a flat list of same-typed integers
  # invites.
  class SecurityTermsInput < GraphQL::Schema::InputObject
    graphql_name 'SecurityTermsInput'
    description 'What is being offered, and on what terms.'

    argument :issuer_entity_id, ID, required: true
    argument :borrower_entity_id, ID, required: true
    argument :name, String, required: true
    argument :principal_minor_units, GraphQL::Types::BigInt, required: true,
                                                             description: 'The offering size.'
    argument :annual_rate_bps, Integer, required: true,
                                        description: 'Simple interest rate, in basis points.'
    argument :term_days, Integer, required: true
  end
end
