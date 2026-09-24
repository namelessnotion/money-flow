import { mount } from '@vue/test-utils'
import { ApolloClient, InMemoryCache } from '@apollo/client/core'
import { MockLink } from '@apollo/client/testing'
import { DefaultApolloClient } from '@vue/apollo-composable'
import { createMemoryHistory } from 'vue-router'
import type { Component } from 'vue'
import { makeRouter } from '../router'

type Mocks = ConstructorParameters<typeof MockLink>[0]

// Mounts `component` against a mocked GraphQL link and an in-memory router
// sitting at `path`, with any child components replaced by `stubs`.
export async function mountWithApollo(
  component: Component,
  {
    mocks,
    props = {},
    path = '/',
    stubs = {},
  }: { mocks: Mocks; props?: Record<string, unknown>; path?: string; stubs?: Record<string, Component> },
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
      stubs,
      provide: { [DefaultApolloClient as unknown as string]: client },
    },
  })
}
