# typed: strict
# frozen_string_literal: true

# The app is loaded by sorbet/tapioca/extensions/load_app.rb, which runs before
# compilers are required — see that file for why it can't live here.
return unless defined?(Sequel::Model)

module Tapioca
  module Compilers
    # Generates typed column accessors, finders, and a typed dataset class for
    # `Sequel::Model` subclasses.
    #
    # Also emits the `Elem` type template that `Sequel::Model`'s `extend ::Enumerable`
    # forces every subclass to re-declare at `typed: strict`. That declaration lives
    # *only* here, never in Ruby source: `extend T::Generic` in source puts
    # `T::Generic#[]` (which returns `self`) ahead of `Sequel::Model::ClassMethods#[]`
    # in the singleton ancestry, silently breaking primary-key lookup at runtime. RBI
    # files are never loaded by Ruby, so there's no runtime effect here.
    #
    # `fixed:` is load-bearing: it keeps bare `Models::Entity` legal in type position,
    # and it keeps `Entity[pk]` dispatching to the real `[]` method rather than being
    # read as a generic type application.
    #
    # Same technique tapioca itself uses in `dsl/compilers/config.rb` for `Config::Options`.
    class SequelModel < Tapioca::Dsl::Compiler
      ConstantType = type_member { { fixed: T.class_of(::Sequel::Model) } }

      # Sequel's `db_schema[:type]` vocabulary — see `Sequel::Database#schema_column_type`.
      # Anything absent (`:interval`, `:composite`, custom PG types) falls back to T.untyped.
      RUBY_TYPES = T.let(
        {
          integer: 'Integer',
          string: 'String',
          boolean: 'T::Boolean',
          float: 'Float',
          decimal: 'BigDecimal',
          date: 'Date',
          time: 'Time',
          blob: 'String', # Sequel::SQL::Blob subclasses String
          enum: 'String'
        }.freeze,
        T::Hash[Symbol, String]
      )

      # Postgres types Sequel has no `:type` for (it reports `type: nil`), keyed by
      # the column's `db_type`. The pg adapter returns a uuid as its String form.
      DB_TYPES = T.let({ 'uuid' => 'String' }.freeze, T::Hash[String, String])

      # Query methods that return a new dataset, so chaining stays typed.
      # `select` deliberately shadows `Enumerable#select` here — Sequel overrides it
      # with the SQL builder, so this matches runtime.
      #
      # `eager` lives on `Sequel::Model::Associations::DatasetMethods`, a module
      # `extend`ed onto a model's dataset instance at runtime rather than included
      # in `Sequel::Dataset` itself — the exact reason it's invisible to Sorbet
      # unless listed here explicitly.
      CHAINABLE = T.let(
        %w[where exclude filter order order_by reverse limit offset select
           select_append distinct group group_by having eager for_update].freeze,
        T::Array[String]
      )

      class << self
        extend T::Sig

        sig { override.returns(T::Enumerable[T::Module[T.anything]]) }
        def gather_constants
          descendants_of(::Sequel::Model).select { |klass| concrete_model?(klass) }
        end

        private

        # Anonymous classes (the intermediates `Sequel::Model(:table)` creates) have no
        # name for tapioca to derive a filename from. Dataset-less classes are abstract
        # bases — `Model.dataset` raises `Sequel::Error` when unset.
        sig { params(klass: T.class_of(::Sequel::Model)).returns(T::Boolean) }
        def concrete_model?(klass)
          return false if Tapioca::Runtime::Reflection.name_of(klass).nil?

          !T.unsafe(klass).dataset.nil?
        rescue ::Sequel::Error
          false
        end
      end

      sig { override.void }
      def decorate
        schema = T.let(
          T.unsafe(constant).db_schema,
          T.nilable(T::Hash[Symbol, T::Hash[Symbol, T.untyped]])
        )
        return if schema.nil? || schema.empty?

        model = "::#{name_of(constant)}"
        dataset = "#{model}::PrivateDataset"

        root.create_path(constant) do |klass|
          klass.create_extend('T::Generic')
          klass.create_type_variable('Elem', type: 'type_template', fixed: model)

          klass.create_module('GeneratedAttributeMethods') { |mod| add_columns(mod, schema) }
          klass.create_include('GeneratedAttributeMethods')

          klass.create_module('GeneratedClassMethods') { |mod| add_class_methods(mod, model, dataset) }
          klass.create_extend('GeneratedClassMethods')

          klass.create_module('GeneratedDatasetMethods') { |mod| add_dataset_methods(mod, model, dataset) }

          # Static-only fiction: no such class exists at runtime, where `where` returns a
          # plain Sequel::Dataset. Mirrors tapioca's ActiveRecord `PrivateRelation` — the
          # name signals "never reference this directly from Ruby source".
          klass.create_class('PrivateDataset', superclass_name: '::Sequel::Dataset') do |ds|
            ds.create_extend('T::Generic')
            ds.create_type_variable('Elem', type: 'type_member', fixed: model)
            ds.create_include('GeneratedDatasetMethods')
          end
        end
      end

      private

      sig { params(mod: RBI::Scope, schema: T::Hash[Symbol, T::Hash[Symbol, T.untyped]]).void }
      def add_columns(mod, schema)
        schema.each do |column, info|
          getter = getter_type(info)
          setter = nilable(getter)

          mod.create_method(column.to_s, return_type: getter)
          mod.create_method(
            "#{column}=",
            parameters: [create_param('value', type: setter)],
            return_type: setter
          )
        end
      end

      sig { params(mod: RBI::Scope, model: String, dataset: String).void }
      def add_class_methods(mod, model, dataset)
        rest = create_rest_param('args', type: 'T.untyped')
        blk = create_block_param('block', type: 'T.untyped')

        mod.create_method('[]', parameters: [rest], return_type: "T.nilable(#{model})")
        mod.create_method('with_pk', parameters: [create_param('pk', type: 'T.untyped')],
                                     return_type: "T.nilable(#{model})")
        mod.create_method('with_pk!', parameters: [create_param('pk', type: 'T.untyped')],
                                      return_type: model)

        # `create` is `new(values, &block).save`; with the default raise_on_save_failure
        # this either returns the model or raises.
        mod.create_method(
          'create',
          parameters: [
            create_opt_param('values', type: 'T::Hash[Symbol, T.untyped]', default: 'T.unsafe(nil)'),
            create_block_param('block', type: "T.nilable(T.proc.params(arg0: #{model}).void)")
          ],
          return_type: model
        )

        mod.create_method('first', parameters: [rest, blk], return_type: "T.nilable(#{model})")
        mod.create_method('first!', parameters: [rest, blk], return_type: model)
        mod.create_method('last', parameters: [rest, blk], return_type: "T.nilable(#{model})")
        mod.create_method('all', parameters: [blk], return_type: "T::Array[#{model}]")
        mod.create_method('count', parameters: [rest, blk], return_type: 'Integer')
        mod.create_method('empty?', return_type: 'T::Boolean')
        mod.create_method('dataset', return_type: dataset)

        add_chainables(mod, dataset)
      end

      sig { params(mod: RBI::Scope, model: String, dataset: String).void }
      def add_dataset_methods(mod, model, dataset)
        rest = create_rest_param('args', type: 'T.untyped')
        blk = create_block_param('block', type: 'T.untyped')

        mod.create_method('first', parameters: [rest, blk], return_type: "T.nilable(#{model})")
        mod.create_method('first!', parameters: [rest, blk], return_type: model)
        mod.create_method('last', parameters: [rest, blk], return_type: "T.nilable(#{model})")
        mod.create_method('single_record', return_type: "T.nilable(#{model})")
        mod.create_method('with_pk', parameters: [create_param('pk', type: 'T.untyped')],
                                     return_type: "T.nilable(#{model})")
        mod.create_method('with_pk!', parameters: [create_param('pk', type: 'T.untyped')],
                                      return_type: model)
        mod.create_method('all', parameters: [blk], return_type: "T::Array[#{model}]")
        mod.create_method('to_a', return_type: "T::Array[#{model}]")
        mod.create_method(
          'each',
          parameters: [create_block_param('block', type: "T.proc.params(arg0: #{model}).void")],
          return_type: dataset
        )
        mod.create_method('count', parameters: [rest, blk], return_type: 'Integer')
        mod.create_method('empty?', return_type: 'T::Boolean')

        # `Sequel::Dataset` includes ::Enumerable without declaring `Elem`, so the
        # `Elem` we fix on PrivateDataset doesn't flow into inherited Enumerable
        # methods. Type the common ones explicitly; tapioca infers the
        # `type_parameters(:U)` clause from the parameter/return types.
        %w[map flat_map].each do |name|
          mod.create_method(
            name,
            parameters: [
              create_block_param('block', type: "T.proc.params(arg0: #{model}).returns(T.type_parameter(:U))")
            ],
            return_type: 'T::Array[T.type_parameter(:U)]'
          )
        end

        add_chainables(mod, dataset)
      end

      sig { params(mod: RBI::Scope, dataset: String).void }
      def add_chainables(mod, dataset)
        CHAINABLE.each do |name|
          mod.create_method(
            name,
            parameters: [
              create_rest_param('args', type: 'T.untyped'),
              create_block_param('block', type: 'T.untyped')
            ],
            return_type: dataset
          )
        end
      end

      sig { params(info: T::Hash[Symbol, T.untyped]).returns(String) }
      def getter_type(info)
        base = if info[:type] == :datetime
                 # `Time` by default; `DateTime` if Sequel.datetime_class was reconfigured.
                 T.unsafe(::Sequel).datetime_class.name.to_s
               else
                 RUBY_TYPES.fetch(info[:type]) { DB_TYPES.fetch(info[:db_type].to_s, 'T.untyped') }
               end

        return base if base == 'T.untyped'

        info[:allow_null] ? "T.nilable(#{base})" : base
      end

      sig { params(type: String).returns(String) }
      def nilable(type)
        type.start_with?('T.nilable(', 'T.untyped') ? type : "T.nilable(#{type})"
      end
    end
  end
end
