# frozen_string_literal: true
# typed: strict

module LendingSimulation
  # A distribution given as its deciles — p0, p10, … p100 — and sampled by
  # inverse transform: a uniform point between two deciles is interpolated
  # linearly between them. Enough shape to reproduce an observed spread from
  # eleven numbers, without pretending to a parametric fit nobody measured.
  class Quantiles < T::Struct
    const :deciles, T::Array[Integer]

    sig { params(values: T::Array[Integer]).returns(Quantiles) }
    def self.of(values)
      raise ArgumentError, "a decile table is eleven values, got #{values.size}" unless values.size == 11
      raise ArgumentError, "deciles must be ascending, got #{values.inspect}" unless values.each_cons(2).all? do |a, b|
        T.must(a) <= T.must(b)
      end

      new(deciles: values.dup.freeze)
    end

    sig { params(random: Random).returns(Integer) }
    def sample(random)
      point = random.rand * 10
      index = [point.floor, 9].min
      low = deciles.fetch(index)
      high = deciles.fetch(index + 1)
      (low + ((high - low) * (point - index))).round
    end

    # Every decile times `factor`, rounded to a whole unit.
    sig { params(factor: Float).returns(Quantiles) }
    def scaled(factor)
      Quantiles.of(deciles.map { |value| (value * factor).round })
    end
  end
end
