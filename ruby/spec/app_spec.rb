# frozen_string_literal: true

require 'spec_helper'
require 'rack/mock'
require_relative '../app/app'

RSpec.describe App do
  subject(:app) { described_class.new }

  let(:mock_request) { Rack::MockRequest.new(app) }

  describe 'GET /healthz' do
    it 'returns 200 ok' do
      response = mock_request.get('/healthz')

      expect(response.status).to eq(200)
      expect(JSON.parse(response.body)).to eq('status' => 'ok')
    end
  end

  describe 'POST /graphql' do
    let(:onboard_entity_query) do
      <<~GRAPHQL
        mutation($name: String!, $role: EntityRole!) {
          onboardEntity(name: $name, role: $role) {
            entity { id name }
          }
        }
      GRAPHQL
    end

    let(:onboard_entity_variables) { { name: 'Test Entity', role: 'INVESTOR' } }

    let(:holder_uuid) { SecureRandom.uuid_v7 }
    let(:entity) { create(:entity, name: 'Test Entity', holder_uuid: holder_uuid) }

    before do
      service_double = instance_double(Services::OnboardEntity)
      allow(Services::OnboardEntity).to receive(:new).and_return(service_double)
      allow(service_double).to receive(:call).and_return(
        OnboardEntityResponse.new(success: true, entity_id: entity.id, holder_uuid: holder_uuid)
      )
    end

    it 'returns 200 for a simple query' do
      response = mock_request.post(
        '/graphql',
        input: JSON.generate(query: '{ ok }'),
        'CONTENT_TYPE' => 'application/json'
      )

      expect(response.status).to eq(200)

      body = JSON.parse(response.body)
      expect(body['errors']).to be_nil
      expect(body['data']).to eq('ok' => true)
    end

    it 'returns 200 for a mutation defined on the schema' do
      response = mock_request.post(
        '/graphql',
        input: JSON.generate(query: onboard_entity_query, variables: onboard_entity_variables),
        'CONTENT_TYPE' => 'application/json'
      )

      expect(response.status).to eq(200)

      body = JSON.parse(response.body)
      expect(body['errors']).to be_nil
      expect(body.dig('data', 'onboardEntity', 'entity', 'name')).to eq('Test Entity')
    end

    it 'returns 200 for a multiplexed request with a mutation' do
      response = mock_request.post(
        '/graphql',
        input: JSON.generate(
          [
            { query: onboard_entity_query, variables: onboard_entity_variables },
            { query: onboard_entity_query, variables: onboard_entity_variables }
          ]
        ),
        'CONTENT_TYPE' => 'application/json'
      )

      expect(response.status).to eq(200)

      body = JSON.parse(response.body)
      expect(body).to be_an(Array)
      expect(body.length).to eq(2)

      body.each do |result|
        expect(result['errors']).to be_nil
        expect(result.dig('data', 'onboardEntity', 'entity', 'name')).to eq('Test Entity')
      end
    end

    it 'returns 400 for invalid JSON' do
      response = mock_request.post(
        '/graphql',
        input: 'not json',
        'CONTENT_TYPE' => 'application/json'
      )

      expect(response.status).to eq(400)
      expect(JSON.parse(response.body)['errors']).not_to be_empty
    end
  end

  describe 'unknown routes' do
    it 'returns 404' do
      response = mock_request.get('/nope')

      expect(response.status).to eq(404)
    end
  end

  describe 'CORS' do
    let(:allowed_origin) { 'https://app.local.namelessnotion.com' }

    describe 'OPTIONS /graphql (preflight)' do
      it 'allows the configured origin' do
        response = mock_request.options(
          '/graphql',
          'HTTP_ORIGIN' => allowed_origin,
          'HTTP_ACCESS_CONTROL_REQUEST_METHOD' => 'POST'
        )

        expect(response.status).to eq(204)
        expect(response.headers['access-control-allow-origin']).to eq(allowed_origin)
        expect(response.headers['access-control-allow-methods']).to include('POST')
      end

      it 'omits the allow-origin header for other origins' do
        response = mock_request.options(
          '/graphql',
          'HTTP_ORIGIN' => 'https://evil.example.com',
          'HTTP_ACCESS_CONTROL_REQUEST_METHOD' => 'POST'
        )

        expect(response.headers['access-control-allow-origin']).to be_nil
      end
    end

    describe 'POST /graphql' do
      it 'includes the allow-origin header for the configured origin' do
        response = mock_request.post(
          '/graphql',
          input: JSON.generate(query: '{ ok }'),
          'CONTENT_TYPE' => 'application/json',
          'HTTP_ORIGIN' => allowed_origin
        )

        expect(response.headers['access-control-allow-origin']).to eq(allowed_origin)
      end

      it 'omits the allow-origin header for other origins' do
        response = mock_request.post(
          '/graphql',
          input: JSON.generate(query: '{ ok }'),
          'CONTENT_TYPE' => 'application/json',
          'HTTP_ORIGIN' => 'https://evil.example.com'
        )

        expect(response.headers['access-control-allow-origin']).to be_nil
      end
    end
  end
end
