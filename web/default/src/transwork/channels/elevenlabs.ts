import { TRANSWORK_BRAND } from '../brand'

export const TRANSWORK_CHANNEL_TYPES = {
  58: 'ElevenLabs',
} as const

export const TRANSWORK_CHANNEL_TYPE_DISPLAY_ORDER = [58] as const

export const TRANSWORK_TYPE_TO_KEY_PROMPT: Record<number, string> = {
  58: 'Enter your ElevenLabs API key',
}

export const TRANSWORK_CHANNEL_TYPE_ICONS: Record<number, string> = {
  58: 'Suno',
}

export const TRANSWORK_CHANNEL_TYPE_CONFIGS = {
  58: {
    id: 58,
    name: 'ElevenLabs',
    icon: 'suno',
    defaultBaseUrl: 'https://api.elevenlabs.io',
    supportedModels: ['scribe_v2', 'scribe_v2_realtime'],
    hints: {
      baseUrl: 'Default: https://api.elevenlabs.io',
      key: 'ElevenLabs API Key',
      models: 'scribe_v2,scribe_v2_realtime',
      other: `Use this channel for ${TRANSWORK_BRAND} realtime and batch ASR routing`,
    },
  },
}
