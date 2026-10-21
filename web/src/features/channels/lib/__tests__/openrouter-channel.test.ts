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
import { expect, test } from 'vitest'

import { channelSchema } from '../../types'
import {
  CHANNEL_FORM_DEFAULT_VALUES,
  channelFormSchema,
  transformChannelToFormDefaults,
  transformFormDataToCreatePayload,
} from '../channel-form'

test.each([false, true])(
  'OpenRouter native Messages round-trips %s without losing existing configuration',
  (enabled) => {
    const form = channelFormSchema.parse({
      ...CHANNEL_FORM_DEFAULT_VALUES,
      name: 'OpenRouter',
      type: 20,
      key: 'test-key',
      models: 'anthropic/claude-sonnet-4.6',
      base_url: 'https://gateway.example/api',
      model_mapping: '{"alias":"anthropic/claude-sonnet-4.6"}',
      is_enterprise_account: true,
      openrouter_native_messages: enabled,
      settings: '{"custom_option":true}',
    })
    const payload = transformFormDataToCreatePayload(form).channel
    expect(JSON.parse(payload.settings ?? '{}')).toMatchObject({
      custom_option: true,
      openrouter_enterprise: true,
      openrouter_native_messages: enabled,
    })
    expect(payload.base_url).toBe(form.base_url)
    expect(payload.model_mapping).toBe(form.model_mapping)
    const channel = channelSchema.parse({
      ...payload,
      id: 7,
      status: 1,
      created_time: 1,
      test_time: 0,
      response_time: 0,
      balance_updated_time: 0,
      other: '',
      remark: '',
    })
    const defaults = transformChannelToFormDefaults(channel)
    expect(defaults.openrouter_native_messages).toBe(enabled)
    expect(defaults.is_enterprise_account).toBe(true)
    const legacy = channelSchema.parse({
      ...channel,
      settings: '{"openrouter_enterprise":false}',
    })
    expect(
      transformChannelToFormDefaults(legacy).openrouter_native_messages
    ).toBe(false)
  }
)

test('changing away from OpenRouter clears both provider settings', () => {
  const form = channelFormSchema.parse({
    ...CHANNEL_FORM_DEFAULT_VALUES,
    name: 'OpenAI',
    type: 1,
    key: 'test-key',
    models: 'gpt-4.1',
    openrouter_native_messages: true,
    settings:
      '{"openrouter_enterprise":true,"openrouter_native_messages":true}',
  })
  const settings = JSON.parse(
    transformFormDataToCreatePayload(form).channel.settings ?? '{}'
  )
  expect(settings).not.toHaveProperty('openrouter_enterprise')
  expect(settings).not.toHaveProperty('openrouter_native_messages')
})

test.each([false, true])(
  'OpenRouter cost billing round-trips %s',
  (enabled) => {
    const form = channelFormSchema.parse({
      ...CHANNEL_FORM_DEFAULT_VALUES,
      name: 'OpenRouter',
      type: 20,
      key: 'test-key',
      models: 'openai/gpt-4.1',
      openrouter_cost_billing: enabled,
      settings: '{"custom_option":true}',
    })
    const payload = transformFormDataToCreatePayload(form).channel
    expect(JSON.parse(payload.settings ?? '{}')).toMatchObject({
      custom_option: true,
      openrouter_cost_billing: enabled,
    })
    const channel = channelSchema.parse({
      ...payload,
      id: 7,
      status: 1,
      created_time: 1,
      test_time: 0,
      response_time: 0,
      balance_updated_time: 0,
      other: '',
      remark: '',
    })
    expect(
      transformChannelToFormDefaults(channel).openrouter_cost_billing
    ).toBe(enabled)
    expect(
      transformChannelToFormDefaults({ ...channel, settings: '{}' })
        .openrouter_cost_billing
    ).toBe(false)
  }
)

test.each([1, 20])(
  'unsupported channel type %s or enterprise disables cost billing',
  (type) => {
    const form = channelFormSchema.parse({
      ...CHANNEL_FORM_DEFAULT_VALUES,
      name: 'Channel',
      type,
      key: 'test-key',
      models: 'openai/gpt-4.1',
      openrouter_cost_billing: true,
      is_enterprise_account: true,
      settings: '{"openrouter_cost_billing":true}',
    })
    const settings = JSON.parse(
      transformFormDataToCreatePayload(form).channel.settings ?? '{}'
    )
    if (type === 20) expect(settings.openrouter_cost_billing).toBe(false)
    else expect(settings).not.toHaveProperty('openrouter_cost_billing')
  }
)
