import { mount } from '@vue/test-utils'
import { ApolloClient, InMemoryCache } from '@apollo/client/core'
import { MockLink } from '@apollo/client/testing'
import { DefaultApolloClient } from '@vue/apollo-composable'
import { createMemoryHistory } from 'vue-router'
import type { Component } from 'vue'
import { makeRouter } from '../router'

type Mocks = ConstructorParameters<typeof MockLink>[0]

// Mounts `component` against a mocked GraphQL link and an in-memory router
// sitting at `path`.
export async function mountWithApollo(
  component: Component,
  { mocks, props = {}, path = '/' }: { mocks: Mocks; props?: Record<string, unknown>; path?: string },
) {
  const client = new ApolloClient({
    link: new MockLink(mocks, { defaultOptions: { delay: 0 } }),
    cache: new InMemoryCache(),
  })
  const router = makeRouter(createMemoryHistory())
  await router.push(path)
  await router.isReady()

  return mount(component, {
    props,
    global: {
      plugins: [router],
      provide: { [DefaultApolloClient as unknown as string]: client },
    },
  })
}
