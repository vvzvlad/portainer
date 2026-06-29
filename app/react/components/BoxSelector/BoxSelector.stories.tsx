import { Meta } from '@storybook/react-webpack5';
import { useState } from 'react';
import { Anchor, Briefcase } from 'lucide-react';

import Docker from '@/assets/ico/vendor/docker.svg?c';

import { BoxSelector } from './BoxSelector';
import { BoxSelectorOption } from './types';

const meta: Meta = {
  title: 'Components/BoxSelector',
  component: BoxSelector,
};

export default meta;

export { Example };

function Example() {
  const [value, setValue] = useState(3);
  const options: BoxSelectorOption<number>[] = [
    {
      description: 'description 1',
      icon: Anchor,
      iconType: 'badge',
      id: '1',
      value: 3,
      label: 'option 1',
    },
    {
      description: 'description 2',
      icon: Briefcase,
      iconType: 'badge',
      id: '2',
      value: 4,
      label: 'option 2',
    },
  ];

  return (
    <BoxSelector
      radioName="name"
      onChange={(value: number) => {
        setValue(value);
      }}
      value={value}
      options={options}
    />
  );
}

export function MultiSelect() {
  const [value, setValue] = useState([3]);
  const options: BoxSelectorOption<number>[] = [
    {
      description: 'description 1',
      icon: Anchor,
      iconType: 'badge',
      id: '1',
      value: 1,
      label: 'option 1',
    },
    {
      description: 'description 2',
      icon: Briefcase,
      iconType: 'badge',
      id: '2',
      value: 2,
      label: 'option 2',
    },
    {
      description: 'description 3',
      icon: Docker,
      id: '3',
      value: 3,
      label: 'option 2',
    },
  ];

  return (
    <BoxSelector
      isMulti
      radioName="name"
      onChange={(value: number[]) => {
        setValue(value);
      }}
      value={value}
      options={options}
    />
  );
}

export function SlimMultiSelect() {
  const [value, setValue] = useState([3]);
  const options: BoxSelectorOption<number>[] = [
    {
      description: 'description 1',
      icon: Anchor,
      iconType: 'badge',
      id: '1',
      value: 1,
      label: 'option 1',
    },
    {
      description: 'description 2',
      icon: Briefcase,
      iconType: 'badge',
      id: '2',
      value: 2,
      label: 'option 2',
    },
    {
      description: 'description 3',
      icon: Docker,
      id: '3',
      value: 3,
      label: 'option 2',
    },
  ];

  return (
    <BoxSelector
      isMulti
      radioName="name"
      onChange={(value: number[]) => {
        setValue(value);
      }}
      value={value}
      options={options}
      slim
    />
  );
}
