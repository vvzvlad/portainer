import { baseHref } from '@/portainer/helpers/pathHelper';
import providers, { getProviderByUrl } from './providers';

const MS_TENANT_ID_PLACEHOLDER = 'TENANT_ID';

export default class OAuthSettingsController {
  /* @ngInject */
  constructor($scope, $async) {
    Object.assign(this, { $scope, $async });

    this.state = {
      provider: 'custom',
      overrideConfiguration: false,
      microsoftTenantID: '',
    };

    this.$onInit = this.$onInit.bind(this);
    this.onSelectProvider = this.onSelectProvider.bind(this);
    this.onMicrosoftTenantIDChange = this.onMicrosoftTenantIDChange.bind(this);
    this.useDefaultProviderConfiguration = this.useDefaultProviderConfiguration.bind(this);
    this.updateSSO = this.updateSSO.bind(this);
    this.onChangeAuthStyle = this.onChangeAuthStyle.bind(this);
    this.onAutoUserProvisionChange = this.onAutoUserProvisionChange.bind(this);
  }

  onAutoUserProvisionChange(value) {
    this.$scope.$evalAsync(() => {
      this.settings.OAuthAutoCreateUsers = value;
    });
  }

  onMicrosoftTenantIDChange() {
    const tenantID = this.state.microsoftTenantID || MS_TENANT_ID_PLACEHOLDER;

    this.settings.AuthorizationURI = `https://login.microsoftonline.com/${tenantID}/oauth2/v2.0/authorize`;
    this.settings.AccessTokenURI = `https://login.microsoftonline.com/${tenantID}/oauth2/v2.0/token`;
    this.settings.ResourceURI = `https://graph.microsoft.com/v1.0/me`;
    this.settings.LogoutURI = `https://login.microsoftonline.com/${tenantID}/oauth2/v2.0/logout`;
  }

  useDefaultProviderConfiguration(providerId) {
    const provider = providers[providerId];

    this.state.overrideConfiguration = false;

    this.settings.AuthorizationURI = provider.authUrl;
    this.settings.AccessTokenURI = provider.accessTokenUrl;
    this.settings.ResourceURI = provider.resourceUrl;
    this.settings.LogoutURI = provider.logoutUrl;
    this.settings.UserIdentifier = provider.userIdentifier;
    this.settings.Scopes = provider.scopes;
    this.settings.AuthStyle = provider.authStyle;

    if (providerId === 'microsoft' && this.state.microsoftTenantID !== '') {
      this.onMicrosoftTenantIDChange();
    }
  }

  onSelectProvider(provider) {
    this.state.provider = provider;

    this.useDefaultProviderConfiguration(provider);
  }

  updateSSO(checked) {
    this.$scope.$evalAsync(() => {
      this.settings.SSO = checked;
    });
  }

  onChangeAuthStyle(val) {
    this.$scope.$evalAsync(() => {
      this.settings.AuthStyle = val;
    });
  }

  $onInit() {
    if (this.settings.RedirectURI === '') {
      this.settings.RedirectURI = window.location.origin + baseHref();
    }

    if (this.settings.AuthorizationURI) {
      const authUrl = this.settings.AuthorizationURI;

      this.state.provider = getProviderByUrl(authUrl);
      if (this.state.provider === 'microsoft') {
        const tenantID = authUrl.match(/login.microsoftonline.com\/(.*?)\//)[1];
        if (tenantID !== MS_TENANT_ID_PLACEHOLDER) {
          this.state.microsoftTenantID = tenantID;
          this.onMicrosoftTenantIDChange();
        }
      }
    }

    if (this.settings.DefaultTeamID === 0) {
      this.settings.DefaultTeamID = null;
    }
  }
}
