import js from '@eslint/js'
import tseslint from 'typescript-eslint'
import hooks from 'eslint-plugin-react-hooks'
import jsxA11y from 'eslint-plugin-jsx-a11y'

export default tseslint.config(
  { ignores: ['dist/**', 'playwright-report/**', 'test-results/**'] },
  js.configs.recommended,
  ...tseslint.configs.recommended,
  {
    files: ['src/**/*.{ts,tsx}'],
    plugins: { 'react-hooks': hooks, 'jsx-a11y': jsxA11y },
    rules: {
      ...hooks.configs.recommended.rules,
      ...jsxA11y.configs.recommended.rules,
      // Named scroll regions need keyboard focus to expose off-screen code.
      'jsx-a11y/no-noninteractive-tabindex': [
        'error',
        { roles: ['tabpanel', 'region'] },
      ],
      'no-restricted-syntax': [
        'error',
        {
          selector:
            'JSXOpeningElement[name.name="div"]:has(JSXAttribute[name.name="aria-label"]):not(:has(JSXAttribute[name.name="role"]))',
          message:
            'Give a named container a semantic role, such as group or region.',
        },
      ],
    },
    settings: {
      'jsx-a11y': { components: { Input: 'input', Button: 'button' } },
    },
  },
)
