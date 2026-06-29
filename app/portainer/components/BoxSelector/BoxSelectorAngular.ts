import { IComponentOptions, IComponentController, IScope } from 'angular';

class BoxSelectorController implements IComponentController {
  onChange!: (value: string | number) => void;

  radioName!: string;

  $scope: IScope;

  /* @ngInject */
  constructor($scope: IScope) {
    this.handleChange = this.handleChange.bind(this);

    this.$scope = $scope;
  }

  handleChange(value: string | number) {
    this.$scope.$evalAsync(() => {
      this.onChange(value);
    });
  }
}

export const BoxSelectorAngular: IComponentOptions = {
  template: `<box-selector-react
    value="$ctrl.value"
    on-change="$ctrl.handleChange"
    options="$ctrl.options"
    radio-name="$ctrl.radioName"
    slim="$ctrl.slim"
    label="$ctrl.label"
  ></box-selector-react>`,
  bindings: {
    value: '<',
    onChange: '<',
    options: '<',
    radioName: '<',
    slim: '<',
    label: '<',
  },
  controller: BoxSelectorController,
};
