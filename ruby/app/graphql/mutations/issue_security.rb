# frozen_string_literal: true
# typed: strict

require_relative 'security_mutation'
require_relative '../types/inputs/security_terms_input'

module Mutations
  # Opens a Security for subscription: its wallets, its terms, and the
  # Transaction that mints its Supply.
  class IssueSecurity < SecurityMutation
    description 'Offer a Security in a Borrower’s debt obligation, minting its Supply.'

    argument :terms, Types::SecurityTermsInput, required: true

    sig { params(terms: Types::SecurityTermsInput).returns(T::Hash[Symbol, T.untyped]) }
    def resolve(terms:)
      answering_with_security { Services::Securities::IssueOffering.new.call(request: request_from(terms)) }
    end

    private

    sig { params(terms: Types::SecurityTermsInput).returns(Services::Securities::IssueOffering::Request) }
    def request_from(terms)
      Services::Securities::IssueOffering::Request.new(
        issuer_entity_id: Integer(terms[:issuer_entity_id], 10),
        borrower_entity_id: Integer(terms[:borrower_entity_id], 10),
        name: terms[:name],
        principal_minor_units: terms[:principal_minor_units],
        annual_rate_bps: terms[:annual_rate_bps],
        term_days: terms[:term_days]
      )
    end
  end
end
