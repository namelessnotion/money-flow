# frozen_string_literal: true
# typed: strict

module LendingSimulation
  # A Profile file as YAML parsed it, narrowed into typed values one key at a
  # time. A missing key or a value of the wrong kind fails here, naming the
  # key, rather than as a nil somewhere in the middle of a run.
  class ProfileConfig
    Grade = Profile::Grade
    InvestorBehaviour = Profile::InvestorBehaviour
    MarketBehaviour = Profile::MarketBehaviour

    sig { params(raw: T.anything).void }
    def initialize(raw)
      @raw = T.let(hash(raw, 'the profile'), T::Hash[String, T.anything])
    end

    sig { params(key: String, from: T::Hash[String, T.anything]).returns(Quantiles) }
    def quantiles(key, from: @raw)
      values = from.fetch(key)
      case values
      when Array then Quantiles.of(values.map { |value| whole(value, key) })
      else raise ArgumentError, "#{key} must be a list of whole numbers"
      end
    end

    sig { params(key: String).returns(Float) }
    def scale(key) = number(hash(@raw.fetch('scale'), 'scale').fetch(key), "scale.#{key}")

    # A mapping of choice to weight, with integer choices (e.g. months).
    sig { params(key: String).returns(T::Hash[Integer, Integer]) }
    def weights(key)
      hash(@raw.fetch(key), key).to_h { |choice, weight| [Integer(choice), whole(weight, "#{key}.#{choice}")] }
    end

    sig { returns(T::Array[Grade]) }
    def grades
      hash(@raw.fetch('grades'), 'grades').map do |name, grade|
        fields = hash(grade, "grades.#{name}")
        Grade.new(name: name, weight: whole(fields.fetch('weight'), "grades.#{name}.weight"),
                  annual_rate_bps: quantiles('annual_rate_bps', from: fields))
      end
    end

    sig { returns(InvestorBehaviour) }
    def investor
      fields = hash(@raw.fetch('investor'), 'investor')
      InvestorBehaviour.new(
        initial_deposit: quantiles('initial_deposit_minor_units', from: fields),
        top_up_probability_per_day: number(fields.fetch('top_up_probability_per_day'), 'top_up_probability'),
        top_up: quantiles('top_up_minor_units', from: fields),
        withdrawal_probability_per_day: number(fields.fetch('withdrawal_probability_per_day'), 'withdrawal'),
        withdrawal_share_percent: quantiles('withdrawal_share_percent', from: fields)
      )
    end

    sig { returns(MarketBehaviour) }
    def market
      fields = hash(@raw.fetch('market'), 'market')
      MarketBehaviour.new(
        offerings_per_day: number(fields.fetch('offerings_per_day'), 'offerings_per_day'),
        buyers_per_offering_per_day: whole(fields.fetch('buyers_per_offering_per_day'), 'buyers_per_offering'),
        minimum_days_drawn: whole(fields.fetch('minimum_days_drawn'), 'minimum_days_drawn')
      )
    end

    private

    sig { params(value: T.anything, what: String).returns(T::Hash[String, T.anything]) }
    def hash(value, what)
      case value
      when Hash then value.transform_keys(&:to_s)
      else raise ArgumentError, "#{what} must be a mapping"
      end
    end

    sig { params(value: T.anything, what: String).returns(Integer) }
    def whole(value, what)
      case value
      when Integer then value
      else raise ArgumentError, "#{what} must be a whole number"
      end
    end

    sig { params(value: T.anything, what: String).returns(Float) }
    def number(value, what)
      case value
      when Numeric then Float(value)
      else raise ArgumentError, "#{what} must be a number"
      end
    end
  end
end
