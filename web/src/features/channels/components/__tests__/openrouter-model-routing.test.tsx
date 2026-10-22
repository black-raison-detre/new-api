/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import {
  cleanup,
  render,
  screen,
  waitFor,
  within,
} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, expect, test, vi } from 'vitest'

import { api } from '@/lib/api'

import { ConfigureModelsDialog } from '../dialogs/configure-models-dialog'

const clients: QueryClient[] = []
function renderEditor(
  props: Partial<React.ComponentProps<typeof ConfigureModelsDialog>> = {}
) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  clients.push(client)
  const onApply = vi.fn()
  const onOpenChange = vi.fn()
  const view = render(
    <QueryClientProvider client={client}>
      <ConfigureModelsDialog
        open
        models={['alias']}
        onApply={onApply}
        onOpenChange={onOpenChange}
        openRouter={{
          modelMapping: '{"alias":"vendor/model:nitro"}',
          routing: {},
          connection: {
            type: 20,
            channel_id: 42,
            base_url: 'https://unsaved.example',
            proxy: 'http://proxy.example',
            header_override: '{"X-Test":"preview"}',
          },
        }}
        {...props}
      />
    </QueryClientProvider>
  )
  return { ...view, onApply, onOpenChange, client }
}

afterEach(() => {
  cleanup()
  for (const client of clients.splice(0)) client.clear()
})

async function chooseRouting(
  user: ReturnType<typeof userEvent.setup>,
  mode: string
) {
  await user.click(screen.getByRole('combobox', { name: 'OpenRouter routing' }))
  await user.click(screen.getByRole('option', { name: mode }))
}

test('editing a legacy mapping keeps it untouched until routing is explicitly selected, and cancel discards edits', async () => {
  const user = userEvent.setup()
  const post = vi.spyOn(api, 'post')
  const view = renderEditor()
  expect(screen.getByText('Existing mapping')).toBeVisible()
  await user.click(
    screen.getByRole('button', { name: 'Configure routing for alias' })
  )
  expect(
    screen.getByRole('heading', { name: 'Routing for alias' })
  ).toHaveFocus()
  await chooseRouting(user, 'Floor')
  expect(post).not.toHaveBeenCalled()
  await user.click(screen.getByRole('button', { name: 'Cancel' }))
  expect(view.onApply).not.toHaveBeenCalled()
  expect(view.onOpenChange).toHaveBeenCalledWith(false)
})

test('locking a model discovers endpoints from unsaved connection and applies its exact ID with the mapping atomically', async () => {
  const user = userEvent.setup()
  const post = vi.spyOn(api, 'post').mockResolvedValue({
    data: {
      success: true,
      data: {
        model: 'vendor/model',
        endpoints: [
          {
            id: 'provider/region',
            name: 'Provider Region',
            provider_name: 'Provider',
          },
        ],
        fetched_at: 1,
        stale: false,
      },
    },
  })
  const view = renderEditor({
    openRouter: {
      modelMapping: '{"alias":"middle","middle":"vendor/model:nitro"}',
      routing: {},
      connection: {
        type: 20,
        channel_id: 42,
        base_url: 'https://unsaved.example',
        proxy: 'http://proxy.example',
        header_override: '{"X-Test":"preview"}',
      },
    },
  })
  await user.click(
    screen.getByRole('button', { name: 'Configure routing for alias' })
  )
  await chooseRouting(user, 'Locked provider')
  expect(screen.getByRole('button', { name: 'Apply' })).toBeDisabled()
  await waitFor(() =>
    expect(post).toHaveBeenCalledWith(
      '/api/channel/openrouter/endpoints',
      {
        type: 20,
        channel_id: 42,
        model: 'vendor/model',
        base_url: 'https://unsaved.example',
        proxy: 'http://proxy.example',
        header_override: '{"X-Test":"preview"}',
      },
      expect.objectContaining({ signal: expect.any(AbortSignal) })
    )
  )
  const endpoint = screen.getByRole('combobox', { name: 'Provider endpoint' })
  await user.click(endpoint)
  await user.click(
    await screen.findByRole('option', { name: /Provider Region/ })
  )
  await user.click(screen.getByRole('button', { name: 'Apply' }))
  expect(view.onApply).toHaveBeenCalledWith(['alias'], {
    modelMapping: JSON.stringify(
      { alias: 'vendor/model', middle: 'vendor/model:nitro' },
      null,
      2
    ),
    routing: { alias: { mode: 'locked', provider: 'provider/region' } },
  })
})

test('adding an alias reuses mapping drafts and publishes two independently routed names on Apply', async () => {
  const user = userEvent.setup()
  const view = renderEditor({
    openRouter: {
      modelMapping: '{"alias":"vendor/model"}',
      routing: { alias: { mode: 'nitro' } },
      connection: { type: 20 },
    },
  })
  await user.click(
    screen.getByRole('button', { name: 'Configure routing for alias' })
  )
  await user.click(
    screen.getByRole('button', { name: 'Add another name for this model' })
  )
  expect(screen.getByRole('button', { name: 'Apply' })).toBeDisabled()
  const names = await screen.findAllByRole('combobox', {
    name: 'Request Model Name',
  })
  await user.type(names[1], 'cheap')
  await user.tab()
  await user.click(screen.getByRole('button', { name: 'Done' }))
  await user.click(
    await screen.findByRole('button', { name: 'Configure routing for cheap' })
  )
  await chooseRouting(user, 'Floor')
  await user.click(screen.getByRole('button', { name: 'Apply' }))
  expect(view.onApply).toHaveBeenCalledWith(
    ['alias', 'cheap'],
    expect.objectContaining({
      routing: { alias: { mode: 'nitro' }, cheap: { mode: 'floor' } },
    })
  )
  expect(JSON.parse(view.onApply.mock.calls[0][1].modelMapping)).toEqual({
    alias: 'vendor/model',
    cheap: 'vendor/model',
  })
})

test('unavailable saved endpoints stay visible and can be retained after discovery fails', async () => {
  const user = userEvent.setup()
  vi.spyOn(api, 'post').mockResolvedValue({
    data: { success: false, message: 'Failed to fetch OpenRouter endpoints' },
  })
  const view = renderEditor({
    initialModel: 'alias',
    openRouter: {
      modelMapping: '{"alias":"vendor/model"}',
      routing: { alias: { mode: 'locked', provider: 'saved/endpoint' } },
      connection: { type: 20 },
    },
  })
  expect(
    await screen.findByText('Failed to load provider endpoints')
  ).toBeVisible()
  expect(
    screen.getByRole('combobox', { name: 'Provider endpoint' })
  ).toHaveValue('saved/endpoint')
  expect(screen.getByRole('button', { name: 'Retry' })).toBeEnabled()
  await user.click(screen.getByRole('button', { name: 'Apply' }))
  expect(view.onApply).toHaveBeenCalledWith(
    ['alias'],
    expect.objectContaining({
      routing: { alias: { mode: 'locked', provider: 'saved/endpoint' } },
    })
  )
})

test.each([true, false])(
  'sensitive lock=%s and enterprise disable routing mutations and endpoint discovery',
  async (locked) => {
    const post = vi.spyOn(api, 'post')
    renderEditor({
      disabled: locked,
      openRouter: {
        modelMapping: '',
        routing: {},
        connection: { type: 20 },
        enterprise: !locked,
      },
    })
    expect(
      screen.getByRole('button', { name: 'Configure routing for alias' })
    ).toBeDisabled()
    if (locked) {
      expect(screen.getByRole('button', { name: 'Apply' })).toBeDisabled()
      for (const checkbox of screen.getAllByRole('checkbox')) {
        expect(checkbox).toHaveAttribute('aria-disabled', 'true')
      }
    } else {
      expect(
        screen.getByText(
          'Per-model routing is unavailable for enterprise accounts.'
        )
      ).toBeVisible()
    }
    expect(post).not.toHaveBeenCalled()
  }
)

test('unchecking one alias removes its policy while keeping other aliases and unrelated saved policy entries', async () => {
  const user = userEvent.setup()
  const view = renderEditor({
    models: ['alias', 'other'],
    openRouter: {
      modelMapping: '{"alias":"vendor/model","other":"vendor/model"}',
      routing: {
        alias: { mode: 'nitro' },
        other: { mode: 'floor' },
        orphan: { mode: 'standard' },
      },
      connection: { type: 20 },
    },
  })
  await user.click(screen.getByRole('checkbox', { name: 'alias' }))
  await user.click(screen.getByRole('button', { name: 'Apply' }))
  expect(view.onApply).toHaveBeenCalledWith(
    ['other'],
    expect.objectContaining({
      routing: { other: { mode: 'floor' }, orphan: { mode: 'standard' } },
    })
  )
})

test('keyboard routing selection updates one alias and closing the detail restores row focus', async () => {
  const user = userEvent.setup()
  const view = renderEditor({
    openRouter: {
      modelMapping: '{"alias":"vendor/model"}',
      routing: {},
      connection: { type: 20 },
    },
  })
  const trigger = screen.getByRole('button', {
    name: 'Configure routing for alias',
  })
  trigger.focus()
  await user.keyboard('{Enter}')
  const mode = screen.getByRole('combobox', { name: 'OpenRouter routing' })
  mode.focus()
  await user.keyboard('{ArrowDown}{Home}{ArrowDown}{Enter}')
  await waitFor(() => expect(mode).toHaveTextContent('Nitro'))
  await user.click(
    within(screen.getByRole('region', { name: 'Routing for alias' })).getByRole(
      'button',
      { name: 'Close' }
    )
  )
  expect(trigger).toHaveFocus()
  await user.click(screen.getByRole('button', { name: 'Apply' }))
  expect(view.onApply).toHaveBeenCalledWith(
    ['alias'],
    expect.objectContaining({ routing: { alias: { mode: 'nitro' } } })
  )
})

test('switching models cancels endpoint discovery and ignores the previous model response', async () => {
  const user = userEvent.setup()
  type Response = {
    data: {
      success: boolean
      data: {
        model: string
        endpoints: { id: string; name: string; provider_name: string }[]
        fetched_at: number
        stale: boolean
      }
    }
  }
  const finish: ((response: Response) => void)[] = []
  const signals: AbortSignal[] = []
  const post = vi
    .spyOn(api, 'post')
    .mockImplementation((_url, _body, config) => {
      signals.push(config?.signal as AbortSignal)
      return new Promise<Response>((resolve) => finish.push(resolve))
    })
  renderEditor({
    models: ['alias', 'other'],
    initialModel: 'alias',
    openRouter: {
      modelMapping: '{"alias":"vendor/a","other":"vendor/b"}',
      routing: {
        alias: { mode: 'locked', provider: 'saved-a' },
        other: { mode: 'locked', provider: 'saved-b' },
      },
      connection: { type: 20 },
    },
  })
  await waitFor(() => expect(post).toHaveBeenCalledTimes(1))
  await user.click(
    screen.getByRole('button', { name: 'Configure routing for other' })
  )
  await waitFor(() => expect(post).toHaveBeenCalledTimes(2))
  expect(signals[0].aborted).toBe(true)
  expect(post.mock.calls[1][1]).toMatchObject({ model: 'vendor/b' })
  finish[0]({
    data: {
      success: true,
      data: {
        model: 'vendor/a',
        endpoints: [
          { id: 'wrong', name: 'Old endpoint', provider_name: 'Old' },
        ],
        fetched_at: 1,
        stale: false,
      },
    },
  })
  finish[1]({
    data: {
      success: true,
      data: {
        model: 'vendor/b',
        endpoints: [
          { id: 'saved-b', name: 'Current endpoint', provider_name: 'Current' },
        ],
        fetched_at: 2,
        stale: false,
      },
    },
  })
  const selector = screen.getByRole('combobox', { name: 'Provider endpoint' })
  await waitFor(() => expect(selector).toHaveValue('Current endpoint'))
  await user.click(selector)
  expect(
    await screen.findByRole('option', { name: /Current endpoint/ })
  ).toBeVisible()
  expect(
    screen.queryByRole('option', { name: /Old endpoint/ })
  ).not.toBeInTheDocument()
})

test('unsaved connection changes cancel endpoint discovery without placing credentials in query keys', async () => {
  const signals: AbortSignal[] = []
  const post = vi
    .spyOn(api, 'post')
    .mockImplementation((_url, _body, config) => {
      signals.push(config?.signal as AbortSignal)
      return new Promise(() => {})
    })
  const config = {
    modelMapping: '{"alias":"vendor/model"}',
    routing: { alias: { mode: 'locked' as const, provider: 'saved' } },
    connection: {
      type: 20 as const,
      channel_id: 42,
      base_url: 'https://first.example',
    },
  }
  const view = renderEditor({ initialModel: 'alias', openRouter: config })
  await waitFor(() => expect(post).toHaveBeenCalledTimes(1))
  view.rerender(
    <QueryClientProvider client={view.client}>
      <ConfigureModelsDialog
        open
        models={['alias']}
        initialModel='alias'
        onApply={view.onApply}
        onOpenChange={view.onOpenChange}
        openRouter={{
          ...config,
          connection: {
            ...config.connection,
            key: 'unsaved-replacement-secret',
            base_url: 'https://second.example',
          },
        }}
      />
    </QueryClientProvider>
  )
  await waitFor(() => expect(post).toHaveBeenCalledTimes(2))
  expect(signals[0].aborted).toBe(true)
  expect(post.mock.calls[1][1]).toMatchObject({
    key: 'unsaved-replacement-secret',
    base_url: 'https://second.example',
    model: 'vendor/model',
  })
  expect(
    JSON.stringify(
      view.client
        .getQueryCache()
        .getAll()
        .map((query) => query.queryKey)
    )
  ).not.toContain('unsaved-replacement-secret')
  view.unmount()
  expect(signals[1].aborted).toBe(true)
})

test('alias chains show the final legacy target and new aliases use its base without changing the existing chain', async () => {
  const user = userEvent.setup()
  const mapping = '{"alias":"middle","middle":"vendor/model:nitro"}'
  const view = renderEditor({
    openRouter: {
      modelMapping: mapping,
      routing: {},
      connection: { type: 20 },
    },
  })
  expect(screen.getByText('Existing mapping')).toBeVisible()
  await user.click(
    screen.getByRole('button', { name: 'Configure routing for alias' })
  )
  expect(screen.getByText('Upstream model: vendor/model:nitro')).toBeVisible()
  await user.click(
    screen.getByRole('button', { name: 'Add another name for this model' })
  )
  await waitFor(() =>
    expect(
      screen.getAllByRole('combobox', { name: 'Upstream Model Name' })
    ).toHaveLength(3)
  )
  expect(
    screen.getAllByRole('combobox', { name: 'Upstream Model Name' })[2]
  ).toHaveValue('vendor/model')
  await user.type(
    screen.getAllByRole('combobox', { name: 'Request Model Name' })[2],
    'second'
  )
  await user.tab()
  await user.click(screen.getByRole('button', { name: 'Done' }))
  await user.click(screen.getByRole('button', { name: 'Apply' }))
  const draft = view.onApply.mock.calls[0]?.[1]
  expect(JSON.parse(draft.modelMapping)).toEqual({
    alias: 'middle',
    middle: 'vendor/model:nitro',
    second: 'vendor/model',
  })
  expect(draft.routing).toEqual({})
})

test('a cyclic model mapping blocks routing edits and Apply without fetching endpoints', () => {
  const post = vi.spyOn(api, 'post')
  renderEditor({
    initialModel: 'alias',
    openRouter: {
      modelMapping: '{"alias":"middle","middle":"alias"}',
      routing: { alias: { mode: 'locked', provider: 'saved' } },
      connection: { type: 20 },
    },
  })
  expect(screen.getByText('Mapping cycle')).toBeVisible()
  expect(
    screen.getByText(
      'Model mapping contains a cycle. Fix the mapping before configuring routing.'
    )
  ).toBeVisible()
  expect(
    screen.getByRole('button', { name: 'Configure routing for alias' })
  ).toBeDisabled()
  expect(screen.getByRole('button', { name: 'Apply' })).toBeDisabled()
  expect(post).not.toHaveBeenCalled()
})

test('a malformed manual endpoint is marked invalid and cannot be applied', async () => {
  const user = userEvent.setup()
  vi.spyOn(api, 'post').mockResolvedValue({
    data: {
      success: true,
      data: {
        model: 'vendor/model',
        endpoints: [],
        fetched_at: 1,
        stale: false,
      },
    },
  })
  renderEditor({
    initialModel: 'alias',
    openRouter: {
      modelMapping: '{"alias":"vendor/model"}',
      routing: { alias: { mode: 'locked', provider: 'valid-endpoint' } },
      connection: { type: 20 },
    },
  })
  const endpoint = screen.getByRole('combobox', { name: 'Provider endpoint' })
  await user.clear(endpoint)
  await user.type(endpoint, 'provider?query')
  await user.tab()
  expect(
    screen.getByText(
      'Enter a valid provider endpoint ID (up to 256 characters).'
    )
  ).toBeVisible()
  expect(endpoint).toHaveAttribute('aria-invalid', 'true')
  expect(screen.getByRole('button', { name: 'Apply' })).toBeDisabled()
})

test('read-only routing allows model selection and applies no sensitive draft even with a malformed saved policy', async () => {
  const user = userEvent.setup()
  const view = renderEditor({
    models: ['alias', 'keep'],
    openRouter: {
      modelMapping: '{"alias":"vendor/model"}',
      routing: { alias: { mode: 'nitro', provider: 'invalid-saved-lock' } },
      connection: { type: 20 },
      readOnly: true,
    },
  })
  expect(
    screen.getByRole('button', { name: 'Configure routing for alias' })
  ).toBeDisabled()
  await user.click(screen.getByRole('checkbox', { name: 'alias' }))
  expect(screen.getByRole('button', { name: 'Apply' })).toBeEnabled()
  await user.click(screen.getByRole('button', { name: 'Apply' }))
  expect(view.onApply).toHaveBeenCalledExactlyOnceWith(['keep'])
})
