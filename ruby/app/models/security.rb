# frozen_string_literal: true
# typed: strict

module Models
  # A claim on one Borrower's debt obligation, offered by an Issuer and bought
  # in fractions by Investors. Terms and the ids it was sent to Go under;
  # lifecycle state is Go's and is read from the projections, never stored here.
  class Security < Sequel::Model
    # The id is chosen by Ruby before Go is called, so the ids derived from it
    # are known before anything is written.
    unrestrict_primary_key

    many_to_one :issuer, class: 'Models::Entity', key: :issuer_entity_id
    many_to_one :borrower, class: 'Models::Entity', key: :borrower_entity_id

    # The Security's own three wallets. Its Issuer's accounts are not among
    # them — those hang off the entity, with a null security_id.
    one_to_many :accounts

    one_to_many :subscriptions
    one_to_many :repayments
  end
end
