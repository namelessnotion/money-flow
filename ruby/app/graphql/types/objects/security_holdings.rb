# frozen_string_literal: true
# typed: strict

require_relative 'base_object'

module Types
  # Everything Types::Security answers by folding its Positions: what has been
  # sold, what is left, what is still owed, and which phase of its life that
  # adds up to.
  #
  # Kept together because they share one load. All four read `held`, which goes
  # through the Dataloader source, so a page of Securities costs a fixed number
  # of queries rather than two per row — and so that a future field folding the
  # same Positions is written here rather than re-querying them.
  module SecurityHoldings
    extend T::Helpers

    # `object` and `dataloader` are the enclosing type's, from graphql-ruby.
    # Saying so here is what lets this stay `typed: strict` outside the class
    # it is mixed into.
    requires_ancestor { Types::BaseObject }

    sig { returns(T::Array[Services::Securities::Positions::Position]) }
    def positions = held

    sig { returns(Integer) }
    def subscribed_minor_units = held.sum(&:principal_minor_units)

    sig { returns(Integer) }
    def outstanding_principal_minor_units = held.sum(&:outstanding_principal_minor_units)

    # Never negative: the read model lags, so it can see a Security sold past
    # its offering size before it sees why.
    sig { returns(Integer) }
    def remaining_minor_units = [object.principal_minor_units - subscribed_minor_units, 0].max

    sig { returns(Services::Securities::Stage::Name) }
    def stage = Services::Securities::Stage.current(snapshot)

    sig { returns(T::Array[Services::Securities::Stage::Step]) }
    def stages = Services::Securities::Stage.of(snapshot)

    private

    # The Dataloader is fiber-based, so `load` hands back the value itself
    # rather than a promise to unwrap.
    sig { returns(T::Array[Services::Securities::Positions::Position]) }
    def held
      @held = T.let(@held, T.nilable(T::Array[Services::Securities::Positions::Position]))
      @held ||= dataloader.with(Sources::SecurityPositions).load(object.id)
    end

    # Built from the joined row and the already-loaded Positions rather than
    # re-read, so rendering a Security costs no extra lookups.
    sig { returns(Services::Securities::Stage::Snapshot) }
    def snapshot
      @snapshot = T.let(@snapshot, T.nilable(Services::Securities::Stage::Snapshot))
      @snapshot ||= Services::Securities::Stage.from(
        object, held,
        offering: Types::Enums::TransactionState.try_deserialize(object[:offering_state]),
        draw: Types::Enums::TransactionState.try_deserialize(object[:draw_state]),
        repaid_anything: object[:repaid_anything] == true
      )
    end
  end
end
