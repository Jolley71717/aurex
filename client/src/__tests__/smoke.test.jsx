import { describe, it, expect } from 'vitest';
import { render } from '@testing-library/react';

// Smoke test — proves the harness is wired correctly: vitest runs, jsdom
// provides a document, React + jest-dom matchers + @testing-library/react
// all resolve. Component-specific tests live next to their components.
describe('vitest harness', () => {
  it('renders a React element into jsdom', () => {
    const { getByText } = render(<div>hello</div>);
    expect(getByText('hello')).toBeInTheDocument();
  });

  it('exposes a document via jsdom', () => {
    expect(typeof document).toBe('object');
    expect(document.createElement).toBeTypeOf('function');
  });
});
