# frozen_string_literal: true
# typed: strict

require_relative 'errors'

module Services
  module Securities
    # Which Wallet backs each account a Securities shape moves money through —
    # how "from the supply into the investor's claims" becomes the wallet ids
    # Go moves value between.
    #
    # Two scopes, because accounts come in two kinds now: an entity's own
    # (security_id null) and a Security's three (security_id set). They share a
    # table and, for a Security and its Issuer, a Holder — so a lookup that did
    # not say which it meant could answer with the wrong one. That is the whole
    # reason the security_id column exists, and the reason this does not reuse
    # Services::Ach::EntityWallets, whose accounts are all of one kind.
    #
    # Within a scope a type resolves to exactly one wallet: on the Security
    # side because accounts_security_type_unique says so, on the entity side
    # because no role's list repeats a type. The database deliberately allows
    # an entity several accounts of one type all the same, so a duplicate here
    # is a bug rather than a choice, and is raised rather than resolved by
    # whichever the hash happened to keep.
    module Wallets
      AccountType = Types::Enums::AccountType

      # The wallet backing each of the `needed` types among the named entity's
      # own accounts.
      #
      # Both halves of that scope earn their keep. A shape hands over several
      # parties' accounts at once — an Investor's and a Security's, a
      # Borrower's and an Issuer's — so the entity id is what keeps one party
      # from answering for another. And a Security's wallets are opened on its
      # Issuer's Holder and so carry the Issuer's entity id too, which is why
      # the null security_id is checked as well.
      sig do
        params(entity_id: Integer, needed: T::Array[AccountType], accounts: T::Array[Models::Account])
          .returns(T::Hash[AccountType, String])
      end
      def self.of_entity(entity_id, needed, accounts)
        in_scope = accounts.select { |account| account.security_id.nil? && account.entity_id == entity_id }
        resolve(needed, in_scope, "entity #{entity_id}")
      end

      # The wallet backing each of the `needed` types among `accounts` that
      # belong to the named Security.
      sig do
        params(security_id: String, needed: T::Array[AccountType], accounts: T::Array[Models::Account])
          .returns(T::Hash[AccountType, String])
      end
      def self.of_security(security_id, needed, accounts)
        in_scope = accounts.select { |account| account.security_id == security_id }
        resolve(needed, in_scope, "security #{security_id}")
      end

      # `scope` names whose accounts these are, so a failure distinguishes "the
      # issuer has no issuer_control" from "the security has no escrow".
      sig do
        params(needed: T::Array[AccountType], accounts: T::Array[Models::Account], scope: String)
          .returns(T::Hash[AccountType, String])
      end
      def self.resolve(needed, accounts, scope)
        by_type = index(accounts, scope)
        missing = needed.reject { |type| by_type.key?(type) }
        raise MissingAccount, "#{scope} has no #{missing.map(&:serialize).join(', ')} account" if missing.any?

        needed.to_h { |type| [type, by_type.fetch(type)] }
      end
      private_class_method :resolve

      sig do
        params(accounts: T::Array[Models::Account], scope: String).returns(T::Hash[AccountType, String])
      end
      def self.index(accounts, scope)
        accounts.each_with_object({}) do |account, by_type|
          type = AccountType.deserialize(account.type)
          raise MissingAccount, "#{scope} has more than one #{account.type} account" if by_type.key?(type)

          by_type[type] = account.wallet_uuid
        end
      end
      private_class_method :index
    end
  end
end
