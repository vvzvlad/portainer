import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { vi } from 'vitest';

import { GitFormModel } from '../types';

import { ComposePathField } from './ComposePathField';

const defaultProps = {
  value: '',
  onChange: vi.fn(),
  isCompose: true,
  model: {
    SourceId: 1,
    ComposeFilePathInRepository: 'docker-compose.yml',
  } as GitFormModel,
  isDockerStandalone: false,
};

describe('ComposePathField', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('should render with default props', () => {
    render(<ComposePathField {...defaultProps} />);

    expect(screen.getByText('Compose path')).toBeInTheDocument();
    expect(screen.getByRole('textbox')).toBeInTheDocument();
    expect(
      screen.getByPlaceholderText('docker-compose.yml')
    ).toBeInTheDocument();
  });

  it('should show manifest path label when isCompose is false', () => {
    render(<ComposePathField {...defaultProps} isCompose={false} />);

    expect(screen.getByText('Manifest path')).toBeInTheDocument();
    expect(screen.getByPlaceholderText('manifest.yml')).toBeInTheDocument();
  });

  it('should display compose file tip text', () => {
    render(<ComposePathField {...defaultProps} />);

    expect(
      screen.getByText(/Indicate the path to the Compose/)
    ).toBeInTheDocument();
    expect(
      screen.getByText(/requires a yaml, yml, json, or hcl file extension/)
    ).toBeInTheDocument();
  });

  it('should display kubernetes manifest file tip text when not compose', () => {
    render(<ComposePathField {...defaultProps} isCompose={false} />);

    expect(screen.getByText(/Indicate the path to the/)).toBeInTheDocument();
    expect(
      screen.getByRole('link', { name: 'Kubernetes manifest file' })
    ).toHaveAttribute(
      'href',
      'https://kubernetes.io/docs/concepts/overview/working-with-objects/'
    );
  });

  it('should show Docker standalone tip when isDockerStandalone is true', () => {
    render(<ComposePathField {...defaultProps} isDockerStandalone />);

    expect(
      screen.getByText(/To enable rebuilding of an image/)
    ).toBeInTheDocument();
    expect(screen.getByText('pull_policy: build')).toBeInTheDocument();
    expect(
      screen.getByRole('link', { name: 'Docker documentation' })
    ).toHaveAttribute(
      'href',
      'https://docs.docker.com/compose/compose-file/#pull_policy'
    );
  });

  it('should not show Docker standalone tip when isDockerStandalone is false', () => {
    render(<ComposePathField {...defaultProps} isDockerStandalone={false} />);

    expect(
      screen.queryByText(/To enable rebuilding of an image/)
    ).not.toBeInTheDocument();
  });

  it('should call onChange when input value changes', async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();

    render(<ComposePathField {...defaultProps} onChange={onChange} />);

    const input = screen.getByRole('textbox');
    await user.type(input, 'new-compose.yml');

    expect(onChange).toHaveBeenCalled();
  });

  it('should display error message when provided', () => {
    const errorMessage = 'Path is required';
    render(<ComposePathField {...defaultProps} errors={errorMessage} />);

    expect(screen.getByText(errorMessage)).toBeInTheDocument();
  });

  it('should show correct input id and data-cy attributes', () => {
    render(<ComposePathField {...defaultProps} />);

    const input = screen.getByRole('textbox');
    expect(input).toHaveAttribute('id', 'stack_repository_path');
    expect(input).toHaveAttribute('data-cy', 'stack-repository-path-input');
  });

  it('should display the current value in the input', () => {
    const testValue = 'custom-compose.yml';
    render(<ComposePathField {...defaultProps} value={testValue} />);

    const input = screen.getByRole('textbox');
    expect(input).toHaveValue(testValue);
  });
});
